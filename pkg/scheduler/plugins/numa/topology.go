// Copyright 2025 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package numa

import (
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	nrtapi "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2/helper/attribute"
)

// tmPolicy mirrors the kubelet Topology Manager policy reported per node via NRT.
type tmPolicy int

const (
	policyNone tmPolicy = iota
	policyBestEffort
	policyRestricted
	policySingleNUMANode
)

// tmScope mirrors the kubelet Topology Manager scope (container vs pod).
type tmScope int

const (
	scopeContainer tmScope = iota
	scopePod
)

const (
	// Attribute names published by NRT exporters (RTE / NFD topology-updater).
	policyAttribute = "topologyManagerPolicy"
	scopeAttribute  = "topologyManagerScope"

	// Attribute / kubelet flag values.
	policyValueSingleNUMANode = "single-numa-node"
	policyValueRestricted     = "restricted"
	policyValueBestEffort     = "best-effort"
	policyValueNone           = "none"

	scopeValuePod       = "pod"
	scopeValueContainer = "container"

	// numaNodeZoneType is the NRT zone Type for an actual NUMA node (as opposed
	// to sockets, dies, etc.).
	numaNodeZoneType = "Node"
)

// numaZone is one NUMA node's working headroom, seeded from NRT zone Available
// and decremented as tasks commit in-cycle, restored on rollback/eviction.
type numaZone struct {
	id        string
	available map[v1.ResourceName]resource.Quantity
}

func (z *numaZone) clone() *numaZone {
	available := make(map[v1.ResourceName]resource.Quantity, len(z.available))
	for name, qty := range z.available {
		available[name] = qty.DeepCopy()
	}
	return &numaZone{id: z.id, available: available}
}

// nodeTopology is the plugin's per-node working state, rebuilt each session.
type nodeTopology struct {
	policy        tmPolicy
	scope         tmScope
	zones         []*numaZone
	topologyAware sets.Set[v1.ResourceName] // allowlist ∩ resources reported per-zone
}

func cloneZones(zones []*numaZone) []*numaZone {
	out := make([]*numaZone, len(zones))
	for i, z := range zones {
		out[i] = z.clone()
	}
	return out
}

func (nt *nodeTopology) zoneByID(id string) *numaZone {
	for _, z := range nt.zones {
		if z.id == id {
			return z
		}
	}
	return nil
}

// buildNodeTopology converts a raw NRT object into the plugin's working model,
// keeping only NUMA-node zones and only the allowlisted resources actually
// reported per zone.
func buildNodeTopology(nrt *nrtapi.NodeResourceTopology, allowlist sets.Set[v1.ResourceName]) *nodeTopology {
	policy, scope := parsePolicyScope(nrt)

	var zones []*numaZone
	topologyAware := sets.New[v1.ResourceName]()
	for _, zone := range nrt.Zones {
		if zone.Type != numaNodeZoneType {
			continue
		}
		available := map[v1.ResourceName]resource.Quantity{}
		for _, ri := range zone.Resources {
			name := v1.ResourceName(ri.Name)
			if !allowlist.Has(name) {
				continue
			}
			available[name] = ri.Available.DeepCopy()
			topologyAware.Insert(name)
		}
		zones = append(zones, &numaZone{id: zone.Name, available: available})
	}

	return &nodeTopology{
		policy:        policy,
		scope:         scope,
		zones:         zones,
		topologyAware: topologyAware,
	}
}

// parsePolicyScope reads the Topology Manager policy and scope from the NRT
// attributes, falling back to the deprecated TopologyPolicies list.
func parsePolicyScope(nrt *nrtapi.NodeResourceTopology) (tmPolicy, tmScope) {
	if attr, ok := attribute.Get(nrt.Attributes, policyAttribute); ok {
		policy := parsePolicyValue(attr.Value)
		scope := scopeContainer
		if scopeAttr, ok := attribute.Get(nrt.Attributes, scopeAttribute); ok {
			scope = parseScopeValue(scopeAttr.Value)
		}
		return policy, scope
	}
	return parseLegacyPolicies(nrt.TopologyPolicies)
}

func parsePolicyValue(value string) tmPolicy {
	switch strings.ToLower(value) {
	case policyValueSingleNUMANode:
		return policySingleNUMANode
	case policyValueRestricted:
		return policyRestricted
	case policyValueBestEffort:
		return policyBestEffort
	default:
		return policyNone
	}
}

func parseScopeValue(value string) tmScope {
	if strings.ToLower(value) == scopeValuePod {
		return scopePod
	}
	return scopeContainer
}

// parseLegacyPolicies interprets the deprecated TopologyPolicies field, whose
// constants encode both policy and scope.
func parseLegacyPolicies(policies []string) (tmPolicy, tmScope) {
	for _, p := range policies {
		switch nrtapi.TopologyManagerPolicy(p) {
		case nrtapi.SingleNUMANodeContainerLevel:
			return policySingleNUMANode, scopeContainer
		case nrtapi.SingleNUMANodePodLevel:
			return policySingleNUMANode, scopePod
		case nrtapi.Restricted, nrtapi.RestrictedContainerLevel:
			return policyRestricted, scopeContainer
		case nrtapi.RestrictedPodLevel:
			return policyRestricted, scopePod
		case nrtapi.BestEffort, nrtapi.BestEffortContainerLevel:
			return policyBestEffort, scopeContainer
		case nrtapi.BestEffortPodLevel:
			return policyBestEffort, scopePod
		}
	}
	return policyNone, scopeContainer
}
