// Copyright 2025 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package numa

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	nrtapi "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/common_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/node_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/api/pod_info"
	"github.com/kai-scheduler/KAI-scheduler/pkg/scheduler/framework"
)

func TestNUMAPlugin(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "NUMA Plugin Suite")
}

// --- helpers ---

func zone(name string, resources map[v1.ResourceName]string) nrtapi.Zone {
	var infos nrtapi.ResourceInfoList
	for n, available := range resources {
		infos = append(infos, nrtapi.ResourceInfo{
			Name:        string(n),
			Available:   resource.MustParse(available),
			Allocatable: resource.MustParse(available),
			Capacity:    resource.MustParse(available),
		})
	}
	return nrtapi.Zone{Name: name, Type: numaNodeZoneType, Resources: infos}
}

func newNRT(policy, scope string, zones ...nrtapi.Zone) *nrtapi.NodeResourceTopology {
	attrs := nrtapi.AttributeList{{Name: policyAttribute, Value: policy}}
	if scope != "" {
		attrs = append(attrs, nrtapi.AttributeInfo{Name: scopeAttribute, Value: scope})
	}
	return &nrtapi.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Attributes: attrs,
		Zones:      nrtapi.ZoneList(zones),
	}
}

func guaranteedGPUPod(gpus, cpu, memory string) *pod_info.PodInfo {
	return &pod_info.PodInfo{
		UID:                 common_info.PodID("ns/pod"),
		Name:                "pod",
		Namespace:           "ns",
		NodeName:            "node-a",
		ResourceRequestType: pod_info.RequestTypeRegular,
		Pod: &v1.Pod{
			Status: v1.PodStatus{QOSClass: v1.PodQOSGuaranteed},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{
					Resources: v1.ResourceRequirements{Requests: v1.ResourceList{
						resourceGPU:       resource.MustParse(gpus),
						v1.ResourceCPU:    resource.MustParse(cpu),
						v1.ResourceMemory: resource.MustParse(memory),
					}},
				}},
			},
		},
	}
}

func twoContainerGPUPod() *pod_info.PodInfo {
	container := func() v1.Container {
		return v1.Container{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{
			resourceGPU:       resource.MustParse("1"),
			v1.ResourceCPU:    resource.MustParse("2"),
			v1.ResourceMemory: resource.MustParse("4Gi"),
		}}}
	}
	return &pod_info.PodInfo{
		UID:                 common_info.PodID("ns/multi"),
		Name:                "multi",
		Namespace:           "ns",
		NodeName:            "node-a",
		ResourceRequestType: pod_info.RequestTypeRegular,
		Pod: &v1.Pod{
			Status: v1.PodStatus{QOSClass: v1.PodQOSGuaranteed},
			Spec:   v1.PodSpec{Containers: []v1.Container{container(), container()}},
		},
	}
}

func defaultPlugin() *numaPlugin {
	return &numaPlugin{
		allowlist:             defaultAllowlist(),
		evaluator:             singleNUMAEvaluator{},
		mapRestrictedToSingle: true,
		nodes:                 map[string]*nodeTopology{},
		reserved:              map[common_info.PodID][]zoneCharge{},
	}
}

// --- topology parsing ---

var _ = Describe("topology parsing", func() {
	It("parses policy and scope from attributes", func() {
		nrt := newNRT(policyValueSingleNUMANode, scopeValuePod)
		policy, scope := parsePolicyScope(nrt)
		Expect(policy).To(Equal(policySingleNUMANode))
		Expect(scope).To(Equal(scopePod))
	})

	It("defaults scope to container when absent", func() {
		nrt := newNRT(policyValueSingleNUMANode, "")
		_, scope := parsePolicyScope(nrt)
		Expect(scope).To(Equal(scopeContainer))
	})

	It("parses the deprecated TopologyPolicies field", func() {
		nrt := &nrtapi.NodeResourceTopology{
			TopologyPolicies: []string{string(nrtapi.SingleNUMANodePodLevel)},
		}
		policy, scope := parsePolicyScope(nrt)
		Expect(policy).To(Equal(policySingleNUMANode))
		Expect(scope).To(Equal(scopePod))
	})

	It("keeps only NUMA-node zones and allowlisted, per-zone resources", func() {
		nrt := newNRT(policyValueSingleNUMANode, scopeValueContainer,
			zone("node-0", map[v1.ResourceName]string{resourceGPU: "2", v1.ResourceCPU: "8", "example.com/foo": "5"}),
		)
		nrt.Zones = append(nrt.Zones, nrtapi.Zone{Name: "socket-0", Type: "Socket"})

		nt := buildNodeTopology(nrt, defaultAllowlist())
		Expect(nt.zones).To(HaveLen(1))
		Expect(nt.topologyAware.Has(resourceGPU)).To(BeTrue())
		Expect(nt.topologyAware.Has(v1.ResourceCPU)).To(BeTrue())
		Expect(nt.topologyAware.Has("example.com/foo")).To(BeFalse(), "non-allowlisted resource is ignored")
	})
})

// --- evaluator ---

var _ = Describe("singleNUMAEvaluator", func() {
	twoZones := func() []*numaZone {
		return []*numaZone{
			{id: "node-0", available: map[v1.ResourceName]resource.Quantity{resourceGPU: resource.MustParse("1"), v1.ResourceCPU: resource.MustParse("4")}},
			{id: "node-1", available: map[v1.ResourceName]resource.Quantity{resourceGPU: resource.MustParse("4"), v1.ResourceCPU: resource.MustParse("2")}},
		}
	}
	aware := sets.New(resourceGPU, v1.ResourceCPU)

	It("returns the lowest zone that satisfies every resource", func() {
		req := resourceRequests{resourceGPU: resource.MustParse("1"), v1.ResourceCPU: resource.MustParse("2")}
		zones, ok := singleNUMAEvaluator{}.evaluate(twoZones(), aware, req, v1.PodQOSGuaranteed)
		Expect(ok).To(BeTrue())
		Expect(zones).To(HaveLen(1))
		Expect(zones[0].id).To(Equal("node-0"))
	})

	It("rejects when no single zone fits all resources together", func() {
		// GPU=4 only fits node-1; CPU=4 only fits node-0; no common zone.
		req := resourceRequests{resourceGPU: resource.MustParse("4"), v1.ResourceCPU: resource.MustParse("4")}
		_, ok := singleNUMAEvaluator{}.evaluate(twoZones(), aware, req, v1.PodQOSGuaranteed)
		Expect(ok).To(BeFalse())
	})

	It("selects the only zone that fits a large GPU request", func() {
		req := resourceRequests{resourceGPU: resource.MustParse("4"), v1.ResourceCPU: resource.MustParse("1")}
		zones, ok := singleNUMAEvaluator{}.evaluate(twoZones(), aware, req, v1.PodQOSGuaranteed)
		Expect(ok).To(BeTrue())
		Expect(zones[0].id).To(Equal("node-1"))
	})

	It("does not align cpu/memory for non-Guaranteed pods", func() {
		req := resourceRequests{v1.ResourceCPU: resource.MustParse("100")}
		_, ok := singleNUMAEvaluator{}.evaluate(twoZones(), aware, req, v1.PodQOSBurstable)
		Expect(ok).To(BeTrue())
	})
})

// --- shouldHandle ---

var _ = Describe("shouldHandle", func() {
	var np *numaPlugin
	var single *nodeTopology

	BeforeEach(func() {
		np = defaultPlugin()
		single = &nodeTopology{policy: policySingleNUMANode, scope: scopeContainer}
	})

	It("handles a Guaranteed whole-GPU pod on a single-numa-node node", func() {
		Expect(np.shouldHandle(guaranteedGPUPod("1", "4", "8Gi"), single)).To(BeTrue())
	})

	It("passes through when there is no topology for the node", func() {
		Expect(np.shouldHandle(guaranteedGPUPod("1", "4", "8Gi"), nil)).To(BeFalse())
	})

	It("passes through best-effort / none nodes", func() {
		Expect(np.shouldHandle(guaranteedGPUPod("1", "4", "8Gi"), &nodeTopology{policy: policyBestEffort})).To(BeFalse())
		Expect(np.shouldHandle(guaranteedGPUPod("1", "4", "8Gi"), &nodeTopology{policy: policyNone})).To(BeFalse())
	})

	It("maps restricted to single-numa-node conservatively by default", func() {
		Expect(np.shouldHandle(guaranteedGPUPod("1", "4", "8Gi"), &nodeTopology{policy: policyRestricted})).To(BeTrue())
	})

	It("skips restricted nodes when conservative mapping is disabled", func() {
		np.mapRestrictedToSingle = false
		Expect(np.shouldHandle(guaranteedGPUPod("1", "4", "8Gi"), &nodeTopology{policy: policyRestricted})).To(BeFalse())
	})

	It("passes through non-Guaranteed pods", func() {
		pod := guaranteedGPUPod("1", "4", "8Gi")
		pod.Pod.Status.QOSClass = v1.PodQOSBurstable
		Expect(np.shouldHandle(pod, single)).To(BeFalse())
	})

	It("passes through fractional and MIG GPU requests", func() {
		frac := guaranteedGPUPod("1", "4", "8Gi")
		frac.ResourceRequestType = pod_info.RequestTypeFraction
		Expect(np.shouldHandle(frac, single)).To(BeFalse())

		mig := guaranteedGPUPod("1", "4", "8Gi")
		mig.ResourceRequestType = pod_info.RequestTypeMigInstance
		Expect(np.shouldHandle(mig, single)).To(BeFalse())
	})
})

// --- predicate ---

var _ = Describe("predicateFn", func() {
	node := &node_info.NodeInfo{Name: "node-a"}

	buildPlugin := func(nrt *nrtapi.NodeResourceTopology) *numaPlugin {
		np := defaultPlugin()
		np.nodes["node-a"] = buildNodeTopology(nrt, np.allowlist)
		return np
	}

	It("admits a pod that fits one NUMA zone", func() {
		np := buildPlugin(newNRT(policyValueSingleNUMANode, scopeValuePod,
			zone("node-0", map[v1.ResourceName]string{resourceGPU: "2", v1.ResourceCPU: "8", v1.ResourceMemory: "16Gi"}),
			zone("node-1", map[v1.ResourceName]string{resourceGPU: "2", v1.ResourceCPU: "8", v1.ResourceMemory: "16Gi"}),
		))
		Expect(np.predicateFn(guaranteedGPUPod("1", "4", "8Gi"), nil, node)).To(Succeed())
	})

	It("rejects a pod whose GPU and CPU cannot co-locate", func() {
		np := buildPlugin(newNRT(policyValueSingleNUMANode, scopeValuePod,
			zone("node-0", map[v1.ResourceName]string{resourceGPU: "0", v1.ResourceCPU: "8", v1.ResourceMemory: "16Gi"}),
			zone("node-1", map[v1.ResourceName]string{resourceGPU: "2", v1.ResourceCPU: "0", v1.ResourceMemory: "16Gi"}),
		))
		err := np.predicateFn(guaranteedGPUPod("1", "4", "8Gi"), nil, node)
		Expect(err).To(HaveOccurred())
	})

	It("passes through nodes without an NRT entry", func() {
		np := defaultPlugin() // no nodes registered
		Expect(np.predicateFn(guaranteedGPUPod("1", "4", "8Gi"), nil, node)).To(Succeed())
	})

	It("admits a two-container pod across two zones under container scope", func() {
		np := buildPlugin(newNRT(policyValueSingleNUMANode, scopeValueContainer,
			zone("node-0", map[v1.ResourceName]string{resourceGPU: "1", v1.ResourceCPU: "8", v1.ResourceMemory: "16Gi"}),
			zone("node-1", map[v1.ResourceName]string{resourceGPU: "1", v1.ResourceCPU: "8", v1.ResourceMemory: "16Gi"}),
		))
		// Each container needs 1 GPU; together they exceed a single zone (which
		// pod scope would reject) but each fits its own zone.
		Expect(np.predicateFn(twoContainerGPUPod(), nil, node)).To(Succeed())
	})

	It("rejects a single container needing more GPUs than any one zone has", func() {
		np := buildPlugin(newNRT(policyValueSingleNUMANode, scopeValueContainer,
			zone("node-0", map[v1.ResourceName]string{resourceGPU: "1", v1.ResourceCPU: "8", v1.ResourceMemory: "16Gi"}),
			zone("node-1", map[v1.ResourceName]string{resourceGPU: "1", v1.ResourceCPU: "8", v1.ResourceMemory: "16Gi"}),
		))
		Expect(np.predicateFn(guaranteedGPUPod("2", "4", "8Gi"), nil, node)).To(HaveOccurred())
	})
})

// --- in-cycle reservation ---

var _ = Describe("in-cycle reservation", func() {
	node := &node_info.NodeInfo{Name: "node-a"}

	It("decrements the chosen zone on allocate and restores it on deallocate", func() {
		np := defaultPlugin()
		nrt := newNRT(policyValueSingleNUMANode, scopeValuePod,
			zone("node-0", map[v1.ResourceName]string{resourceGPU: "2", v1.ResourceCPU: "8", v1.ResourceMemory: "16Gi"}),
		)
		np.nodes["node-a"] = buildNodeTopology(nrt, np.allowlist)
		z := np.nodes["node-a"].zoneByID("node-0")

		task := guaranteedGPUPod("1", "4", "8Gi")
		np.allocate(&framework.Event{Task: task})

		gpu := z.available[resourceGPU]
		Expect(gpu.Value()).To(Equal(int64(1)))
		Expect(np.reserved).To(HaveKey(task.UID))

		np.deallocate(&framework.Event{Task: task})
		gpu = z.available[resourceGPU]
		Expect(gpu.Value()).To(Equal(int64(2)))
		Expect(np.reserved).ToNot(HaveKey(task.UID))
	})

	It("blocks a second pod once the zone is exhausted, then frees it on rollback", func() {
		np := defaultPlugin()
		nrt := newNRT(policyValueSingleNUMANode, scopeValuePod,
			zone("node-0", map[v1.ResourceName]string{resourceGPU: "1", v1.ResourceCPU: "8", v1.ResourceMemory: "16Gi"}),
		)
		np.nodes["node-a"] = buildNodeTopology(nrt, np.allowlist)

		first := guaranteedGPUPod("1", "4", "8Gi")
		first.UID = "ns/first"
		np.allocate(&framework.Event{Task: first})

		second := guaranteedGPUPod("1", "4", "8Gi")
		second.UID = "ns/second"
		Expect(np.predicateFn(second, nil, node)).To(HaveOccurred(), "zone exhausted by first pod")

		// Rolling back the first pod frees the zone for the second.
		np.deallocate(&framework.Event{Task: first})
		Expect(np.predicateFn(second, nil, node)).To(Succeed())
	})
})
