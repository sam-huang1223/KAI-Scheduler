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
	// restrictedModeArg selects how `restricted` nodes are handled:
	// "conservative" (default), "faithful", or "skip" (see restrictedMode).
	restrictedModeArg = "restrictedMode"
)

// restrictedMode selects how a node running the kubelet `restricted` Topology
// Manager policy is handled.
type restrictedMode int

const (
	// restrictedConservative maps `restricted` to `single-numa-node`: stricter
	// than the kubelet (may reject a genuinely multi-NUMA pod it would admit) but
	// provably never causes a TopologyAffinityError. Default.
	restrictedConservative restrictedMode = iota
	// restrictedFaithful reproduces the kubelet's `restricted` admission decision
	// (common minimal-width mask), admitting multi-NUMA pods. Divergence in the
	// model can cause a TopologyAffinityError; see the v2 caveats in the design.
	restrictedFaithful
	// restrictedSkip leaves `restricted` nodes unconstrained (kubelet is the sole
	// arbiter); the plugin still handles single-numa-node nodes.
	restrictedSkip
)

func parseRestrictedMode(value string) (restrictedMode, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "conservative":
		return restrictedConservative, true
	case "faithful":
		return restrictedFaithful, true
	case "skip":
		return restrictedSkip, true
	default:
		return restrictedConservative, false
	}
}

// zoneCharge records a per-zone decrement applied for one task, so the exact
// reservation can be reversed on rollback/eviction even after headroom changed.
type zoneCharge struct {
	zoneID string
	req    resourceRequests
}

type numaPlugin struct {
	allowlist      sets.Set[v1.ResourceName]
	single         numaEvaluator
	restricted     numaEvaluator
	restrictedMode restrictedMode

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

	mode, ok := parseRestrictedMode(arguments.GetString(restrictedModeArg, ""))
	if !ok {
		log.InfraLogger.Warningf("numa: unrecognized %s %q. Defaulting to conservative",
			restrictedModeArg, arguments.GetString(restrictedModeArg, ""))
	}

	return &numaPlugin{
		allowlist:      allowlist,
		single:         singleNUMAEvaluator{},
		restricted:     restrictedEvaluator{},
		restrictedMode: mode,
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

// evaluatorFor selects the evaluator for a node's policy, and whether the plugin
// engages at all. restricted is routed per the configured mode.
func (np *numaPlugin) evaluatorFor(nt *nodeTopology) (numaEvaluator, bool) {
	switch nt.policy {
	case policySingleNUMANode:
		return np.single, true
	case policyRestricted:
		switch np.restrictedMode {
		case restrictedConservative:
			return np.single, true // treat as single-numa-node (stricter, safe)
		case restrictedFaithful:
			return np.restricted, true
		default: // restrictedSkip
			return nil, false
		}
	default: // best-effort, none
		return nil, false
	}
}

// shouldHandle reports the evaluator to use for this (task, node), or false to
// pass through untouched.
func (np *numaPlugin) shouldHandle(task *pod_info.PodInfo, nt *nodeTopology) (numaEvaluator, bool) {
	if nt == nil {
		return nil, false
	}
	eval, ok := np.evaluatorFor(nt)
	if !ok {
		return nil, false
	}
	if task.Pod == nil || task.Pod.Status.QOSClass != v1.PodQOSGuaranteed {
		return nil, false
	}
	if task.ResourceRequestType != pod_info.RequestTypeRegular {
		return nil, false
	}
	// Whole-GPU only: fractional / MIG GPUs are not NUMA-aligned by the kubelet.
	if task.IsFractionCandidate() || task.IsMigCandidate() {
		return nil, false
	}
	return eval, true
}

func (np *numaPlugin) predicateFn(task *pod_info.PodInfo, _ *podgroup_info.PodGroupInfo, node *node_info.NodeInfo) error {
	nt := np.nodes[node.Name]
	eval, ok := np.shouldHandle(task, nt)
	if !ok {
		return nil
	}

	if _, ok := np.computePlacement(nt, eval, task); !ok {
		log.InfraLogger.V(6).Infof("numa: task <%s/%s> cannot be NUMA-aligned on node <%s>",
			task.Namespace, task.Name, node.Name)
		return common_info.NewFitError(task.Name, task.Namespace, node.Name,
			"node cannot NUMA-align the pod's resources under its Topology Manager policy")
	}
	return nil
}

// computePlacement determines, against the node's current per-zone headroom,
// the zone charges that realize a NUMA-valid placement for the task. It is pure
// (never mutates nt). Returns false when no placement exists.
func (np *numaPlugin) computePlacement(nt *nodeTopology, eval numaEvaluator, task *pod_info.PodInfo) ([]zoneCharge, bool) {
	qos := task.Pod.Status.QOSClass

	if nt.scope == scopePod {
		req := np.podRequests(task)
		zones, ok := eval.evaluate(nt.zones, nt.topologyAware, req, qos)
		if !ok {
			return nil, false
		}
		return splitCharge(zones, req), true
	}

	return np.computeContainerScopePlacement(nt, eval, task, qos)
}

// computeContainerScopePlacement aligns each container independently while
// sharing zone headroom (greedy, first-fit lowest zone), matching the upstream
// container-level handler. Init containers run serially: each is checked but its
// request is not accumulated.
func (np *numaPlugin) computeContainerScopePlacement(nt *nodeTopology, eval numaEvaluator, task *pod_info.PodInfo, qos v1.PodQOSClass) ([]zoneCharge, bool) {
	scratch := cloneZones(nt.zones)
	var charges []zoneCharge

	for _, container := range nonRestartableInitContainers(task.Pod) {
		req := np.containerRequests(container.Resources.Requests)
		if _, ok := eval.evaluate(scratch, nt.topologyAware, req, qos); !ok {
			return nil, false
		}
	}

	for _, container := range accumulatedContainers(task.Pod) {
		req := np.containerRequests(container.Resources.Requests)
		zones, ok := eval.evaluate(scratch, nt.topologyAware, req, qos)
		if !ok {
			return nil, false
		}
		for _, charge := range splitCharge(zones, req) {
			if zone := zoneByID(scratch, charge.zoneID); zone != nil {
				chargeZone(zone, charge.req)
			}
			charges = append(charges, charge)
		}
	}

	return charges, true
}

// splitCharge distributes each requested resource across the chosen zones,
// greedily filling the lowest zone first up to its available headroom. For a
// single zone this charges the whole request to it (exact, as in v1). For a
// multi-zone restricted span the split is approximate — the kubelet does not fix
// the per-zone split at admission either — but it prevents gross within-cycle
// over-placement.
func splitCharge(zones []*numaZone, req resourceRequests) []zoneCharge {
	charges := make([]zoneCharge, len(zones))
	for i, zone := range zones {
		charges[i] = zoneCharge{zoneID: zone.id, req: resourceRequests{}}
	}

	for name, qty := range req {
		remaining := qty.DeepCopy()
		for i, zone := range zones {
			if remaining.Sign() <= 0 {
				break
			}
			take := zone.available[name].DeepCopy()
			if take.Cmp(remaining) > 0 {
				take = remaining.DeepCopy()
			}
			if take.Sign() > 0 {
				charges[i].req[name] = take
				remaining.Sub(take)
			}
		}
		// Feasibility guarantees the span covers the request; defensively drop any
		// residue on the last zone.
		if remaining.Sign() > 0 && len(zones) > 0 {
			last := len(zones) - 1
			q := charges[last].req[name]
			q.Add(remaining)
			charges[last].req[name] = q
		}
	}
	return charges
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
	eval, ok := np.shouldHandle(task, nt)
	if !ok {
		return
	}
	charges, ok := np.computePlacement(nt, eval, task)
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
