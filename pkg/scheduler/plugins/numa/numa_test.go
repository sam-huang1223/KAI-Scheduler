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

// zoneAC builds a zone with explicit available and capacity quantities.
func zoneAC(id string, available, capacity map[v1.ResourceName]string) *numaZone {
	a := map[v1.ResourceName]resource.Quantity{}
	c := map[v1.ResourceName]resource.Quantity{}
	for name, v := range available {
		a[name] = resource.MustParse(v)
	}
	for name, v := range capacity {
		c[name] = resource.MustParse(v)
	}
	return &numaZone{id: id, available: a, capacity: c}
}

func zoneIDs(zones []*numaZone) []string {
	ids := make([]string, 0, len(zones))
	for _, z := range zones {
		ids = append(ids, z.id)
	}
	return ids
}

func defaultPlugin() *numaPlugin {
	return &numaPlugin{
		allowlist:      defaultAllowlist(),
		single:         singleNUMAEvaluator{},
		restricted:     restrictedEvaluator{},
		restrictedMode: restrictedConservative,
		nodes:          map[string]*nodeTopology{},
		reserved:       map[common_info.PodID][]zoneCharge{},
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

	handles := func(p *numaPlugin, task *pod_info.PodInfo, nt *nodeTopology) bool {
		_, ok := p.shouldHandle(task, nt)
		return ok
	}

	BeforeEach(func() {
		np = defaultPlugin()
		single = &nodeTopology{policy: policySingleNUMANode, scope: scopeContainer}
	})

	It("handles a Guaranteed whole-GPU pod on a single-numa-node node", func() {
		Expect(handles(np, guaranteedGPUPod("1", "4", "8Gi"), single)).To(BeTrue())
	})

	It("passes through when there is no topology for the node", func() {
		Expect(handles(np, guaranteedGPUPod("1", "4", "8Gi"), nil)).To(BeFalse())
	})

	It("passes through best-effort / none nodes", func() {
		Expect(handles(np, guaranteedGPUPod("1", "4", "8Gi"), &nodeTopology{policy: policyBestEffort})).To(BeFalse())
		Expect(handles(np, guaranteedGPUPod("1", "4", "8Gi"), &nodeTopology{policy: policyNone})).To(BeFalse())
	})

	It("maps restricted to single-numa-node conservatively by default", func() {
		eval, ok := np.shouldHandle(guaranteedGPUPod("1", "4", "8Gi"), &nodeTopology{policy: policyRestricted})
		Expect(ok).To(BeTrue())
		Expect(eval).To(BeAssignableToTypeOf(singleNUMAEvaluator{}))
	})

	It("uses the faithful evaluator for restricted nodes in faithful mode", func() {
		np.restrictedMode = restrictedFaithful
		eval, ok := np.shouldHandle(guaranteedGPUPod("1", "4", "8Gi"), &nodeTopology{policy: policyRestricted})
		Expect(ok).To(BeTrue())
		Expect(eval).To(BeAssignableToTypeOf(restrictedEvaluator{}))
	})

	It("skips restricted nodes in skip mode", func() {
		np.restrictedMode = restrictedSkip
		Expect(handles(np, guaranteedGPUPod("1", "4", "8Gi"), &nodeTopology{policy: policyRestricted})).To(BeFalse())
	})

	It("passes through non-Guaranteed pods", func() {
		pod := guaranteedGPUPod("1", "4", "8Gi")
		pod.Pod.Status.QOSClass = v1.PodQOSBurstable
		Expect(handles(np, pod, single)).To(BeFalse())
	})

	It("passes through fractional and MIG GPU requests", func() {
		frac := guaranteedGPUPod("1", "4", "8Gi")
		frac.ResourceRequestType = pod_info.RequestTypeFraction
		Expect(handles(np, frac, single)).To(BeFalse())

		mig := guaranteedGPUPod("1", "4", "8Gi")
		mig.ResourceRequestType = pod_info.RequestTypeMigInstance
		Expect(handles(np, mig, single)).To(BeFalse())
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

// --- faithful restricted evaluator ---

var _ = Describe("restrictedEvaluator", func() {
	eval := restrictedEvaluator{}

	It("admits a cpu request that must span both zones; single-numa-node rejects it", func() {
		// Per-zone cpu capacity 96, free 95/96. Request 120 needs 2 zones.
		zones := []*numaZone{
			zoneAC("node-0", map[v1.ResourceName]string{v1.ResourceCPU: "95"}, map[v1.ResourceName]string{v1.ResourceCPU: "96"}),
			zoneAC("node-1", map[v1.ResourceName]string{v1.ResourceCPU: "96"}, map[v1.ResourceName]string{v1.ResourceCPU: "96"}),
		}
		aware := sets.New(v1.ResourceCPU)
		req := resourceRequests{v1.ResourceCPU: resource.MustParse("120")}

		_, ok := singleNUMAEvaluator{}.evaluate(zones, aware, req, v1.PodQOSGuaranteed)
		Expect(ok).To(BeFalse(), "single-numa-node cannot fit 120 in one 96-cpu zone")

		got, ok := eval.evaluate(zones, aware, req, v1.PodQOSGuaranteed)
		Expect(ok).To(BeTrue())
		Expect(zoneIDs(got)).To(ConsistOf("node-0", "node-1"))
	})

	It("rejects the fragmentation footgun: fits one zone's capacity but not its free cpus", func() {
		// Capacity 96/zone (minimal width 1), but free 45/46 -> the only feasible span
		// is 2 zones, which is wider than minimal -> not preferred -> reject.
		zones := []*numaZone{
			zoneAC("node-0", map[v1.ResourceName]string{v1.ResourceCPU: "45"}, map[v1.ResourceName]string{v1.ResourceCPU: "96"}),
			zoneAC("node-1", map[v1.ResourceName]string{v1.ResourceCPU: "46"}, map[v1.ResourceName]string{v1.ResourceCPU: "96"}),
		}
		_, ok := eval.evaluate(zones, sets.New(v1.ResourceCPU), resourceRequests{v1.ResourceCPU: resource.MustParse("80")}, v1.PodQOSGuaranteed)
		Expect(ok).To(BeFalse())
	})

	It("rejects when resources disagree on minimal width (4 GPU needs 2 nodes, 1 CPU needs 1)", func() {
		zones := []*numaZone{
			zoneAC("node-0", map[v1.ResourceName]string{resourceGPU: "2", v1.ResourceCPU: "100"}, map[v1.ResourceName]string{resourceGPU: "2", v1.ResourceCPU: "100"}),
			zoneAC("node-1", map[v1.ResourceName]string{resourceGPU: "2", v1.ResourceCPU: "100"}, map[v1.ResourceName]string{resourceGPU: "2", v1.ResourceCPU: "100"}),
		}
		aware := sets.New(resourceGPU, v1.ResourceCPU)
		req := resourceRequests{resourceGPU: resource.MustParse("4"), v1.ResourceCPU: resource.MustParse("1")}
		_, ok := eval.evaluate(zones, aware, req, v1.PodQOSGuaranteed)
		Expect(ok).To(BeFalse())
	})

	It("admits a large balanced pod where every resource needs the same 2-node mask", func() {
		zones := []*numaZone{
			zoneAC("node-0", map[v1.ResourceName]string{resourceGPU: "4", v1.ResourceCPU: "16"}, map[v1.ResourceName]string{resourceGPU: "4", v1.ResourceCPU: "16"}),
			zoneAC("node-1", map[v1.ResourceName]string{resourceGPU: "4", v1.ResourceCPU: "16"}, map[v1.ResourceName]string{resourceGPU: "4", v1.ResourceCPU: "16"}),
		}
		aware := sets.New(resourceGPU, v1.ResourceCPU)
		req := resourceRequests{resourceGPU: resource.MustParse("6"), v1.ResourceCPU: resource.MustParse("24")}
		got, ok := eval.evaluate(zones, aware, req, v1.PodQOSGuaranteed)
		Expect(ok).To(BeTrue())
		Expect(zoneIDs(got)).To(ConsistOf("node-0", "node-1"))
	})

	It("places a small pod on a single zone (|M|=1 special case)", func() {
		zones := []*numaZone{
			zoneAC("node-0", map[v1.ResourceName]string{resourceGPU: "4", v1.ResourceCPU: "16"}, map[v1.ResourceName]string{resourceGPU: "4", v1.ResourceCPU: "16"}),
			zoneAC("node-1", map[v1.ResourceName]string{resourceGPU: "4", v1.ResourceCPU: "16"}, map[v1.ResourceName]string{resourceGPU: "4", v1.ResourceCPU: "16"}),
		}
		got, ok := eval.evaluate(zones, sets.New(resourceGPU, v1.ResourceCPU),
			resourceRequests{resourceGPU: resource.MustParse("1"), v1.ResourceCPU: resource.MustParse("2")}, v1.PodQOSGuaranteed)
		Expect(ok).To(BeTrue())
		Expect(got).To(HaveLen(1))
		Expect(got[0].id).To(Equal("node-0"))
	})
})

var _ = Describe("faithful restricted: predicate and reservation", func() {
	node := &node_info.NodeInfo{Name: "node-a"}

	cpuOnlyNRT := func() *nrtapi.NodeResourceTopology {
		nrt := newNRT(policyValueRestricted, scopeValuePod,
			zone("node-0", map[v1.ResourceName]string{v1.ResourceCPU: "95"}),
			zone("node-1", map[v1.ResourceName]string{v1.ResourceCPU: "96"}),
		)
		// zone() sets available==capacity from a single value; force capacity 96.
		for i := range nrt.Zones {
			for j := range nrt.Zones[i].Resources {
				nrt.Zones[i].Resources[j].Capacity = resource.MustParse("96")
			}
		}
		return nrt
	}

	cpuPod := func(cpu string) *pod_info.PodInfo {
		p := guaranteedGPUPod("0", cpu, "1Gi")
		delete(p.Pod.Spec.Containers[0].Resources.Requests, resourceGPU)
		delete(p.Pod.Spec.Containers[0].Resources.Limits, resourceGPU)
		return p
	}

	It("conservative mode rejects the multi-NUMA cpu pod (over-strict)", func() {
		np := defaultPlugin() // conservative
		np.nodes["node-a"] = buildNodeTopology(cpuOnlyNRT(), np.allowlist)
		Expect(np.predicateFn(cpuPod("120"), nil, node)).To(HaveOccurred())
	})

	It("faithful mode admits it and splits the reservation across both zones", func() {
		np := defaultPlugin()
		np.restrictedMode = restrictedFaithful
		np.nodes["node-a"] = buildNodeTopology(cpuOnlyNRT(), np.allowlist)
		nt := np.nodes["node-a"]

		task := cpuPod("120")
		Expect(np.predicateFn(task, nil, node)).To(Succeed())

		np.allocate(&framework.Event{Task: task})
		// 120 split greedily: node-0 filled to its 95 free, node-1 takes the rest (25).
		n0 := nt.zoneByID("node-0").available[v1.ResourceCPU]
		n1 := nt.zoneByID("node-1").available[v1.ResourceCPU]
		Expect(n0.Value()).To(Equal(int64(0)))
		Expect(n1.Value()).To(Equal(int64(71)))

		np.deallocate(&framework.Event{Task: task})
		n0 = nt.zoneByID("node-0").available[v1.ResourceCPU]
		n1 = nt.zoneByID("node-1").available[v1.ResourceCPU]
		Expect(n0.Value()).To(Equal(int64(95)))
		Expect(n1.Value()).To(Equal(int64(96)))
	})
})

var _ = Describe("parseRestrictedMode", func() {
	It("parses known modes and defaults empty to conservative", func() {
		for in, want := range map[string]restrictedMode{
			"":             restrictedConservative,
			"conservative": restrictedConservative,
			"faithful":     restrictedFaithful,
			"skip":         restrictedSkip,
			"FAITHFUL":     restrictedFaithful,
		} {
			got, ok := parseRestrictedMode(in)
			Expect(ok).To(BeTrue(), "input %q", in)
			Expect(got).To(Equal(want), "input %q", in)
		}
	})

	It("flags an unrecognized mode and falls back to conservative", func() {
		got, ok := parseRestrictedMode("bogus")
		Expect(ok).To(BeFalse())
		Expect(got).To(Equal(restrictedConservative))
	})
})

var _ = Describe("buildNodeTopology capacity", func() {
	It("records per-zone capacity distinct from available", func() {
		nrt := newNRT(policyValueRestricted, scopeValuePod, zone("node-0", nil))
		nrt.Zones[0].Resources = nrtapi.ResourceInfoList{{
			Name:        string(v1.ResourceCPU),
			Available:   resource.MustParse("40"),
			Allocatable: resource.MustParse("95"),
			Capacity:    resource.MustParse("96"),
		}}
		nt := buildNodeTopology(nrt, defaultAllowlist())
		z := nt.zoneByID("node-0")
		avail := z.available[v1.ResourceCPU]
		capn := z.capacity[v1.ResourceCPU]
		Expect(avail.Value()).To(Equal(int64(40)))
		Expect(capn.Value()).To(Equal(int64(96)))
	})
})
