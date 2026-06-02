// Copyright 2025 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package numa

import (
	"math"
	"math/bits"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// resourceRequests is a per-resource request projected onto the allowlist.
type resourceRequests map[v1.ResourceName]resource.Quantity

// numaEvaluator decides whether a request can be NUMA-aligned on a node and
// returns the zone(s) the in-cycle reservation should charge. Two implementations
// exist behind this seam: singleNUMAEvaluator (single-numa-node, one zone) and
// restrictedEvaluator (faithful `restricted`, possibly several zones).
type numaEvaluator interface {
	// evaluate returns the satisfying zone(s) and whether the request can be
	// aligned. For single-numa-node the slice always holds exactly one zone; for
	// a restricted multi-NUMA span it may hold several.
	evaluate(zones []*numaZone, topologyAware resourceSet, req resourceRequests, qos v1.PodQOSClass) ([]*numaZone, bool)
}

type resourceSet interface {
	Has(v1.ResourceName) bool
}

// singleNUMAEvaluator replicates the kubelet `single-numa-node` admission check
// as a bitmask intersection: one NUMA zone must satisfy every topology-aware
// request.
type singleNUMAEvaluator struct{}

func (singleNUMAEvaluator) evaluate(zones []*numaZone, topologyAware resourceSet, req resourceRequests, qos v1.PodQOSClass) ([]*numaZone, bool) {
	zone, ok := resourcesAvailableInAnyZone(zones, topologyAware, req, qos)
	if !ok {
		return nil, false
	}
	return []*numaZone{zone}, true
}

// resourcesAvailableInAnyZone intersects, across all topology-aware resources,
// the set of zones that can satisfy each one, and returns the lowest-indexed
// surviving zone (mirroring the kubelet picking the narrowest/lowest hint).
func resourcesAvailableInAnyZone(zones []*numaZone, topologyAware resourceSet, req resourceRequests, qos v1.PodQOSClass) (*numaZone, bool) {
	if len(zones) == 0 {
		return nil, false
	}

	candidates := make([]bool, len(zones))
	for i := range candidates {
		candidates[i] = true
	}

	for name, qty := range req {
		if qty.IsZero() || !topologyAware.Has(name) {
			continue
		}
		for i, zone := range zones {
			if !candidates[i] {
				continue
			}
			if !suitable(qos, name, qty, zone.available[name]) {
				candidates[i] = false
			}
		}
	}

	for i, zone := range zones {
		if candidates[i] {
			return zone, true
		}
	}
	return nil, false
}

// suitable reports whether a single zone can satisfy one resource request. For
// non-Guaranteed pods the kubelet does not align cpu/memory/hugepages, so those
// never constrain placement; everything else needs the headroom.
func suitable(qos v1.PodQOSClass, name v1.ResourceName, qty, available resource.Quantity) bool {
	if qos != v1.PodQOSGuaranteed && isHintExemptResource(name) {
		return true
	}
	return available.Cmp(qty) >= 0
}

func isHintExemptResource(name v1.ResourceName) bool {
	switch name {
	case v1.ResourceCPU, v1.ResourceMemory:
		return true
	default:
		return strings.HasPrefix(string(name), string(v1.ResourceHugePagesPrefix))
	}
}

// maxRestrictedZones caps the 2^n NUMA-mask enumeration. Real nodes have a
// handful of NUMA zones; a node reporting more falls back to the conservative
// single-numa path rather than enumerating an intractable number of masks.
const maxRestrictedZones = 12

// zoneMask is a bitset over a node's NUMA zones (bit i ⇔ zones[i]).
type zoneMask uint

// hint is a candidate NUMA-node grouping for one resource, mirroring the
// kubelet's TopologyHint: a mask that can supply the request, flagged Preferred
// when it uses the minimal number of NUMA nodes.
type hint struct {
	mask      zoneMask
	preferred bool
}

// resourceHinter generates the hints for one resource over the given zones,
// mirroring a kubelet hint provider (CPU Manager, Device Manager, ...).
type resourceHinter func(zones []*numaZone, name v1.ResourceName, qty resource.Quantity) []hint

// restrictedEvaluator reproduces the kubelet `restricted` admission decision.
// The kubelet admits iff the best merged hint is Preferred, which (because the
// merge always prefers a preferred permutation when one exists) reduces to:
// there is a NUMA-node mask M that is a preferred (minimal-width, satisfiable)
// hint for *every* topology-aware resource the pod requests. single-numa-node is
// the special case |M| = 1.
type restrictedEvaluator struct{}

func (restrictedEvaluator) evaluate(zones []*numaZone, topologyAware resourceSet, req resourceRequests, qos v1.PodQOSClass) ([]*numaZone, bool) {
	n := len(zones)
	if n == 0 || n > maxRestrictedZones {
		return nil, false
	}

	var common map[zoneMask]struct{}
	constrained := false
	for name, qty := range req {
		if qty.IsZero() || !topologyAware.Has(name) {
			continue
		}
		if qos != v1.PodQOSGuaranteed && isHintExemptResource(name) {
			continue
		}
		preferred := preferredMasks(hinterFor(name)(zones, name, qty))
		if len(preferred) == 0 {
			// No minimal-width hint is satisfiable ⇒ the only feasible spans are
			// wider than minimal ⇒ not preferred ⇒ restricted rejects.
			return nil, false
		}
		if !constrained {
			common = preferred
			constrained = true
			continue
		}
		common = intersectMasks(common, preferred)
		if len(common) == 0 {
			return nil, false
		}
	}

	if !constrained {
		return []*numaZone{zones[0]}, true // nothing topology-aware to align
	}
	return zonesOfMask(zones, narrowestMask(common)), true
}

// hinterFor returns the hint provider for a resource. cpu's minimal width is
// derived from per-zone capacity (the CPU Manager static policy manages a fixed
// per-node pool); device-like resources (gpu, NICs) and memory derive it from
// what is currently available, like the Device / Memory managers. The cpu
// counting model is approximate (it does not account for full physical cores /
// SMT) — see the v2 caveats in the design.
func hinterFor(name v1.ResourceName) resourceHinter {
	if name == v1.ResourceCPU {
		return countingHinter(true)
	}
	return countingHinter(false)
}

// countingHinter generates hints by summing a resource's per-zone quantities
// over every NUMA-node subset. A subset is a feasible hint when its *available*
// sum covers the request; it is preferred when its node count equals the minimal
// width — the fewest nodes whose width-basis sum covers the request.
// widthFromCapacity selects capacity (cpu) vs available (everything else) as the
// width basis, reproducing the kubelet's "minimal width from capacity, feasibility
// from free" rule that drives the fragmentation footgun.
func countingHinter(widthFromCapacity bool) resourceHinter {
	return func(zones []*numaZone, name v1.ResourceName, qty resource.Quantity) []hint {
		n := len(zones)

		minWidth := n + 1
		for m := zoneMask(1); m < (1 << uint(n)); m++ {
			basis := sumOverMask(zones, m, name, widthFromCapacity)
			if basis.Cmp(qty) >= 0 {
				if w := bits.OnesCount(uint(m)); w < minWidth {
					minWidth = w
				}
			}
		}

		var hints []hint
		for m := zoneMask(1); m < (1 << uint(n)); m++ {
			avail := sumOverMask(zones, m, name, false)
			if avail.Cmp(qty) < 0 {
				continue // not satisfiable from available headroom
			}
			hints = append(hints, hint{mask: m, preferred: bits.OnesCount(uint(m)) == minWidth})
		}
		return hints
	}
}

func sumOverMask(zones []*numaZone, mask zoneMask, name v1.ResourceName, fromCapacity bool) resource.Quantity {
	sum := resource.Quantity{}
	for i, zone := range zones {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		if fromCapacity {
			q := zone.capacity[name]
			sum.Add(q)
		} else {
			q := zone.available[name]
			sum.Add(q)
		}
	}
	return sum
}

func preferredMasks(hints []hint) map[zoneMask]struct{} {
	out := map[zoneMask]struct{}{}
	for _, h := range hints {
		if h.preferred {
			out[h.mask] = struct{}{}
		}
	}
	return out
}

func intersectMasks(a, b map[zoneMask]struct{}) map[zoneMask]struct{} {
	out := map[zoneMask]struct{}{}
	for m := range a {
		if _, ok := b[m]; ok {
			out[m] = struct{}{}
		}
	}
	return out
}

// narrowestMask picks the preferred common mask the kubelet would settle on:
// fewest NUMA nodes, then lowest zones.
func narrowestMask(masks map[zoneMask]struct{}) zoneMask {
	best := zoneMask(0)
	bestWidth := math.MaxInt
	for m := range masks {
		w := bits.OnesCount(uint(m))
		if w < bestWidth || (w == bestWidth && m < best) {
			best, bestWidth = m, w
		}
	}
	return best
}

func zonesOfMask(zones []*numaZone, mask zoneMask) []*numaZone {
	var out []*numaZone
	for i, zone := range zones {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, zone)
		}
	}
	return out
}
