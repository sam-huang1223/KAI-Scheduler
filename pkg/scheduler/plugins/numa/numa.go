// Copyright 2025 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

// Package numa implements a filter predicate that replicates the kubelet
// Topology Manager `single-numa-node` admission check against NodeResourceTopology
// (NRT) data, so Guaranteed whole-GPU pods are only placed on nodes where the
// kubelet can actually NUMA-align them. It also tracks per-NUMA-zone consumption
// within a scheduling cycle so several pods placed on one node in one cycle do
// not over-commit a zone. See docs/developer/designs/numa-topology.
package numa

import (
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	resourcehelper "k8s.io/component-helpers/resource"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/common_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/node_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/podgroup_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/log"
)

const pluginName = "numa"

// Plugin arguments.
const (
	// nicResourcesArg is a comma-separated list of NIC extended-resource names
	// to add to the default allowlist (e.g. "nvidia.com/rdma").
	nicResourcesArg = "nicResources"
	// mapRestrictedToSingleArg controls whether `restricted` nodes are handled
	// conservatively as `single-numa-node` (default true) or skipped (false).
	mapRestrictedToSingleArg = "mapRestrictedToSingleNUMANode"
)

// zoneCharge records a per-zone decrement applied for one task, so the exact
// reservation can be reversed on rollback/eviction even after headroom changed.
type zoneCharge struct {
	zoneID string
	req    resourceRequests
}

type numaPlugin struct {
	allowlist             sets.Set[v1.ResourceName]
	evaluator             numaEvaluator
	mapRestrictedToSingle bool

	// Per-cycle working state, (re)built every OnSessionOpen.
	nodes    map[string]*nodeTopology           // node name → topology; absent ⇒ pass
	reserved map[common_info.PodID][]zoneCharge // task UID → applied charges
}

// New builds a numa plugin instance. A fresh instance is created every cycle, so
// all state is per-cycle.
func New(arguments framework.PluginArguments) framework.Plugin {
	allowlist := defaultAllowlist()
	for _, nic := range splitCSV(arguments.GetString(nicResourcesArg, "")) {
		allowlist.Insert(v1.ResourceName(nic))
	}

	mapRestricted, err := arguments.GetBool(mapRestrictedToSingleArg, true)
	if err != nil {
		log.InfraLogger.Warningf("numa: failed to parse %s: %v. Defaulting to true", mapRestrictedToSingleArg, err)
		mapRestricted = true
	}

	return &numaPlugin{
		allowlist:             allowlist,
		evaluator:             singleNUMAEvaluator{},
		mapRestrictedToSingle: mapRestricted,
	}
}

func defaultAllowlist() sets.Set[v1.ResourceName] {
	return sets.New(resourceGPU, v1.ResourceCPU, v1.ResourceMemory)
}

const resourceGPU v1.ResourceName = "nvidia.com/gpu"

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (np *numaPlugin) Name() string {
	return pluginName
}

func (np *numaPlugin) OnSessionOpen(ssn *framework.Session) {
	np.nodes = map[string]*nodeTopology{}
	np.reserved = map[common_info.PodID][]zoneCharge{}

	for name, node := range ssn.ClusterInfo.Nodes {
		if node.NodeResourceTopology == nil {
			continue
		}
		np.nodes[name] = buildNodeTopology(node.NodeResourceTopology, np.allowlist)
	}

	ssn.AddPredicateFn(np.predicateFn)
	ssn.AddEventHandler(&framework.EventHandler{
		AllocateFunc:   np.allocate,
		DeallocateFunc: np.deallocate,
	})
}

func (np *numaPlugin) OnSessionClose(_ *framework.Session) {
	np.nodes = nil
	np.reserved = nil
}

// effectivePolicy maps the raw NRT policy through the conservative `restricted`
// normalization.
func (np *numaPlugin) effectivePolicy(nt *nodeTopology) tmPolicy {
	if nt.policy == policyRestricted && np.mapRestrictedToSingle {
		return policySingleNUMANode
	}
	return nt.policy
}

// shouldHandle reports whether the plugin constrains placement of this task on
// this node. Anything else passes through untouched.
func (np *numaPlugin) shouldHandle(task *pod_info.PodInfo, nt *nodeTopology) bool {
	if nt == nil || np.effectivePolicy(nt) != policySingleNUMANode {
		return false
	}
	if task.Pod == nil || task.Pod.Status.QOSClass != v1.PodQOSGuaranteed {
		return false
	}
	if task.ResourceRequestType != pod_info.RequestTypeRegular {
		return false
	}
	// Whole-GPU only: fractional / MIG GPUs are not NUMA-aligned by the kubelet.
	if task.IsFractionCandidate() || task.IsMigCandidate() {
		return false
	}
	return true
}

func (np *numaPlugin) predicateFn(task *pod_info.PodInfo, _ *podgroup_info.PodGroupInfo, node *node_info.NodeInfo) error {
	nt := np.nodes[node.Name]
	if !np.shouldHandle(task, nt) {
		return nil
	}

	if _, ok := np.computePlacement(nt, task); !ok {
		log.InfraLogger.V(6).Infof("numa: task <%s/%s> cannot be NUMA-aligned on node <%s>",
			task.Namespace, task.Name, node.Name)
		return common_info.NewFitError(task.Name, task.Namespace, node.Name,
			"node cannot satisfy the pod's resources within a single NUMA node")
	}
	return nil
}

// computePlacement determines, against the node's current per-zone headroom,
// the zone charges that realize a NUMA-valid placement for the task. It is pure
// (never mutates nt). Returns false when no placement exists.
func (np *numaPlugin) computePlacement(nt *nodeTopology, task *pod_info.PodInfo) ([]zoneCharge, bool) {
	qos := task.Pod.Status.QOSClass

	if nt.scope == scopePod {
		req := np.podRequests(task)
		zones, ok := np.evaluator.evaluate(nt.zones, nt.topologyAware, req, qos)
		if !ok {
			return nil, false
		}
		return []zoneCharge{{zoneID: zones[0].id, req: req}}, true
	}

	return np.computeContainerScopePlacement(nt, task, qos)
}

// computeContainerScopePlacement aligns each container independently while
// sharing zone headroom (greedy, first-fit lowest zone), matching the upstream
// container-level handler. Init containers run serially: each is checked but its
// request is not accumulated.
func (np *numaPlugin) computeContainerScopePlacement(nt *nodeTopology, task *pod_info.PodInfo, qos v1.PodQOSClass) ([]zoneCharge, bool) {
	scratch := cloneZones(nt.zones)
	var charges []zoneCharge

	for _, container := range nonRestartableInitContainers(task.Pod) {
		req := np.containerRequests(container.Resources.Requests)
		if _, ok := np.evaluator.evaluate(scratch, nt.topologyAware, req, qos); !ok {
			return nil, false
		}
	}

	for _, container := range accumulatedContainers(task.Pod) {
		req := np.containerRequests(container.Resources.Requests)
		zones, ok := np.evaluator.evaluate(scratch, nt.topologyAware, req, qos)
		if !ok {
			return nil, false
		}
		chargeZone(zones[0], req)
		charges = append(charges, zoneCharge{zoneID: zones[0].id, req: req})
	}

	return charges, true
}

// accumulatedContainers returns the containers whose requests run concurrently
// in steady state: regular containers plus native sidecars (restartable init
// containers).
func accumulatedContainers(pod *v1.Pod) []v1.Container {
	containers := make([]v1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == v1.ContainerRestartPolicyAlways {
			containers = append(containers, c)
		}
	}
	containers = append(containers, pod.Spec.Containers...)
	return containers
}

func nonRestartableInitContainers(pod *v1.Pod) []v1.Container {
	var containers []v1.Container
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy == nil || *c.RestartPolicy != v1.ContainerRestartPolicyAlways {
			containers = append(containers, c)
		}
	}
	return containers
}

func (np *numaPlugin) allocate(event *framework.Event) {
	task := event.Task
	nt := np.nodes[task.NodeName]
	if !np.shouldHandle(task, nt) {
		return
	}
	charges, ok := np.computePlacement(nt, task)
	if !ok {
		return
	}
	for _, charge := range charges {
		if zone := nt.zoneByID(charge.zoneID); zone != nil {
			chargeZone(zone, charge.req)
		}
	}
	np.reserved[task.UID] = charges
}

func (np *numaPlugin) deallocate(event *framework.Event) {
	task := event.Task
	charges, ok := np.reserved[task.UID]
	if !ok {
		return
	}
	nt := np.nodes[task.NodeName]
	if nt != nil {
		for _, charge := range charges {
			if zone := nt.zoneByID(charge.zoneID); zone != nil {
				unchargeZone(zone, charge.req)
			}
		}
	}
	delete(np.reserved, task.UID)
}

// podRequests computes the effective pod request projected onto the allowlist,
// accounting for init containers, native sidecars and pod overhead (kubelet's
// pod-scope view).
func (np *numaPlugin) podRequests(task *pod_info.PodInfo) resourceRequests {
	return np.project(resourcehelper.PodRequests(task.Pod, resourcehelper.PodResourcesOptions{}))
}

func (np *numaPlugin) containerRequests(requests v1.ResourceList) resourceRequests {
	return np.project(requests)
}

func (np *numaPlugin) project(list v1.ResourceList) resourceRequests {
	req := resourceRequests{}
	for name, qty := range list {
		if np.allowlist.Has(name) {
			req[name] = qty.DeepCopy()
		}
	}
	return req
}

func chargeZone(zone *numaZone, req resourceRequests) {
	for name, qty := range req {
		avail := zone.available[name]
		avail.Sub(qty)
		zone.available[name] = avail
	}
}

func unchargeZone(zone *numaZone, req resourceRequests) {
	for name, qty := range req {
		avail := zone.available[name]
		avail.Add(qty)
		zone.available[name] = avail
	}
}
