// Copyright 2025 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package numa

import (
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// resourceRequests is a per-resource request projected onto the allowlist.
type resourceRequests map[v1.ResourceName]resource.Quantity

// numaEvaluator decides whether a request can be NUMA-aligned on a node and
// returns the zone(s) the in-cycle reservation should charge. v1 ships a single
// implementation (singleNUMAEvaluator); the seam lets a faithful `restricted`
// evaluator be slotted in later without disturbing the proven-safe path.
type numaEvaluator interface {
	// evaluate returns the satisfying zone(s) and whether the request can be
	// aligned. For single-numa-node the slice always holds exactly one zone.
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
