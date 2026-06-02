# NUMA-Aware Scheduling via NodeResourceTopology

## Summary

This document describes a v1 design for making KAI-Scheduler aware of per-NUMA-node
resource topology, so that **Guaranteed-QoS, whole-GPU workloads** are placed only on
nodes where the kubelet's Topology Manager can actually align their GPU, CPU, memory and
NIC resources onto a single NUMA node.

The scheduler consumes the [`NodeResourceTopology`][nrt-api] (NRT) CRD, which is published
per-node by an external exporter (NFD topology-updater or the resource-topology-exporter).
A new `numa` plugin replicates the kubelet's `single-numa-node` admission check against the
NRT data as a **filter predicate**, and tracks per-NUMA-zone consumption **within a scheduling
cycle** so that multiple pods placed on the same node in one cycle are not over-committed onto
the same zone. Compensating for NRT *staleness across cycles* is an optional extension
([Appendix A](#appendix-a-optional-cross-cycle-staleness-compensation)), not part of v1.

## Motivation

The kubelet's Topology Manager makes the real NUMA-alignment decision at **pod admission
time**, after the scheduler has already chosen a node. When a node is configured with the
`single-numa-node` policy and a Guaranteed pod's resources cannot all be satisfied from one
NUMA node, the kubelet rejects the pod with a `TopologyAffinityError` and the pod returns to
`Pending`. The scheduler then re-attempts — potentially picking the same bad node again —
producing wasted cycles and, in the worst case, a hot loop.

The scheduler cannot *enforce* NUMA alignment (the kubelet owns that), but it can *predict*
it and avoid placing pods where the kubelet will reject them. This is the same role played
by the upstream `NodeResourceTopologyMatch` plugin in kubernetes-sigs/scheduler-plugins.

The highest-value case for KAI is GPU locality: strict GPU↔CPU↔NIC NUMA affinity (e.g. for
GPUDirect RDMA) materially affects throughput for AI/ML workloads, and is exactly the
`single-numa-node` scenario.

## Usage Stories

### GPU + NIC locality for distributed training

A training pod requests one whole GPU, a block of CPUs, memory, and an RDMA NIC. For best
performance all four must sit on the same NUMA node. The cluster runs the kubelet with
`topologyManagerPolicy: single-numa-node`. Today KAI may place the pod on a node whose free
GPU is on a different NUMA node than its free CPUs, and the kubelet rejects it. With this
design KAI filters such nodes out up front.

### Packing many single-GPU pods onto a multi-NUMA node

A node has 8 GPUs split 4+4 across two NUMA nodes, but limited CPUs per NUMA node. KAI places
several single-GPU Guaranteed pods on it in one scheduling cycle. Without per-zone tracking,
KAI's whole-node accounting can approve a layout the kubelet cannot honor. In-cycle NUMA-zone
reservation ensures each successive pod sees the reduced per-zone headroom.

## Goals

- Consume the `NodeResourceTopology` CRD and attach it to the scheduler snapshot.
- Implement a `numa` plugin that filters out nodes where the kubelet would reject a
  Guaranteed, whole-GPU pod under the `single-numa-node` policy.
- Track per-NUMA-zone resource consumption **within a scheduling cycle** so pods placed in
  the same cycle do not over-commit a zone.
- Restrict NUMA reasoning to an explicit, configurable allowlist of resources
  (default `nvidia.com/gpu`, `cpu`, `memory`, plus configured NIC resources).
- Leave the kubelet as the source of truth and enforcement point; the plugin is an
  optimization layer only.

## Non-Goals (v1)

- **Fractional / MIG GPU sharing.** Only whole-GPU (`RequestTypeRegular`, integer
  `nvidia.com/gpu`) Guaranteed pods are handled. Shared-GPU pods are typically not
  Guaranteed QoS, so the kubelet Topology Manager does not align them.
- **Faithful `restricted` policy modeling.** `restricted` permits multi-NUMA spreads; modeling
  it requires reproducing the kubelet's cross-provider hint merge. v1 maps `restricted` →
  `single-numa-node` (conservative — see *Policy handling*). A full design for faithful
  `restricted` is in [v2](#v2-faithful-restricted-via-reimplemented-hint-merge).
- **Cross-cycle NRT staleness compensation.** A freshly-bound pod is reflected in NRT only once
  the exporter republishes. In practice that is near-real-time — both exporters push
  **event-driven** updates on kubelet allocation changes (see *Deployment guidance*) — but it can
  lag up to the **periodic** refresh (default 60s) if event updates are disabled or delayed. v1
  does not compensate for any residual lag; the in-cycle layer only guards a single cycle. An
  optional design is in [Appendix A](#appendix-a-optional-cross-cycle-staleness-compensation).
- **NUMA scoring.** Preferring nodes with tighter NUMA fit is a follow-on; v1 is filter-only.
- **Changes to the binder / `BindRequest`.** The kubelet performs the actual NUMA pinning for
  whole-GPU Guaranteed pods via the device plugins; no device selection is communicated.
- **Extending the resource vectors** to represent per-NUMA pools.

## Background: who decides NUMA alignment

The **kubelet Topology Manager** implements every policy (`none`, `best-effort`,
`restricted`, `single-numa-node`) and enforces it at admission, independently of the
scheduler. NUMA alignment therefore works correctly with zero scheduler support — absent a
scheduler plugin, you simply get more admission failures and reschedule churn.

The scheduler plugin exists only to reduce that churn. This means:

- **Correctness is the kubelet's.** A bug or gap in this plugin can cause extra reschedules,
  never a mis-pinned pod.
- The plugin only needs to target the cases where prediction is cheap and failures common —
  which is exactly `single-numa-node`.

## Design Details

### Policy handling (conservative)

| NRT policy on node | v1 behavior |
| --- | --- |
| `single-numa-node` | Fully modeled: require one NUMA zone to satisfy all allowlisted requests. |
| `restricted` | **Mapped to `single-numa-node`** (conservative). Stricter than the kubelet — may reject a genuinely multi-NUMA pod the kubelet would accept, but provably never causes a `TopologyAffinityError`. |
| `best-effort`, `none` | Pass (no constraint). |
| No NRT object for node | Pass (cluster without NRT is unaffected). |

The `restricted` mapping is a single policy-normalization step; flipping it to "skip"
(upstream behavior) is a one-line change if the conservative behavior proves too strict.
The admit decision is isolated behind a `numaEvaluator` seam (see *Policy evaluator seam*)
so the faithful `restricted` evaluator from
[v2](#v2-faithful-restricted-via-reimplemented-hint-merge) can be slotted in without
disturbing the v1 path.

### NRT ingestion

1. Add a `NodeResourceTopology` lister to the data-lister interface
   (`pkg/scheduler/cache/cluster_info/data_lister`) and register the informer in
   `kubernetes_lister.go`.
2. In `cluster_info.Snapshot()`, attach the raw `*NodeResourceTopology` (matched by node
   name) to the corresponding `NodeInfo` as a pointer field. **No resource-vector changes** —
   this is a raw reference only.

This keeps ingestion consistent with KAI's deterministic, snapshot-based scheduling and
testability, while leaving the vector model untouched.

### Plugin-local per-zone data model

The plugin builds its own working state at `OnSessionOpen` from the snapshot's NRT data and
mutates it during the cycle. The resource vectors are **not** involved.

```go
type tmPolicy int // none | bestEffort | restricted | singleNUMANode
type tmScope  int // container | pod

// One NUMA node's working headroom, seeded from NRT zone Available,
// decremented as tasks commit in-cycle, restored on rollback/eviction.
type numaZone struct {
    id        string
    available map[v1.ResourceName]resource.Quantity
}

type nodeTopology struct {
    policy        tmPolicy
    scope         tmScope
    zones         []*numaZone               // NRT zones of Type == "Node"
    topologyAware sets.Set[v1.ResourceName] // allowlist ∩ resources reported per-zone
}

type numaPlugin struct {
    allowlist sets.Set[v1.ResourceName]    // configurable; default {gpu, cpu, memory, nics}
    nodes     map[string]*nodeTopology     // rebuilt each OnSessionOpen; nil entry ⇒ pass
    reserved  map[common_info.PodID]string // task UID → chosen zone id (for exact restore)
}
```

The scheduler instantiates a **fresh plugin instance every cycle** (`OpenSession` calls the
builder then `OnSessionOpen`), so all plugin state is per-cycle: `nodes` is rebuilt from the
snapshot's NRT data each cycle, and `reserved` tracks only the current cycle's in-flight
allocations. v1 keeps no cross-cycle state (see
[Appendix A](#appendix-a-optional-cross-cycle-staleness-compensation)).

### Resource allowlist

A resource constrains placement only if it is **both** in the configured allowlist **and**
reported per-zone in the node's NRT object. Default allowlist: `nvidia.com/gpu`, `cpu`,
`memory`, plus operator-configured NIC resource names. This converts the one thing the
scheduler cannot infer from NRT — whether a device's plugin actually emits NUMA hints — into
explicit configuration, and keeps the per-zone math over a small, predictable set.

### `shouldHandle` gate

The plugin engages for a task only when **all** hold (otherwise the predicate passes
through):

- node has a `nodeTopology` entry with policy `singleNUMANode` (post-normalization), and
- `task.Pod.Status.QoSClass == Guaranteed`, and
- whole-GPU request: `task.ResourceRequestType == RequestTypeRegular` and integer
  `nvidia.com/gpu` (i.e. `!IsFractionCandidate() && !IsMigCandidate()`).

### Filter algorithm

Following the upstream `single-numa-node` approach: a bitmask intersection rather than a
hint merge.

```
resourcesAvailableInAnyZone(nt, req):       // req limited to nt.topologyAware
    mask = { all zones set }
    for r, qty in req:
        if qty == 0: continue
        zmask = { zone z : suitable(qos, r, qty, z.available[r]) }
        mask = mask AND zmask
        if mask empty: return (nil, false)
    return (lowest set zone, true)           // kubelet picks narrowest/lowest

suitable(qos, r, qty, avail):
    if qos != Guaranteed and r in {cpu, memory, hugepages}: return true  // kubelet won't align
    return avail >= qty
```

**Scope split** (read from the node's NRT attributes):

- **`pod` scope** → align the whole pod to one zone. Use KAI's effective-pod-request
  computation (which already accounts for init containers and native sidecars), projected
  onto the allowlist, and run `resourcesAvailableInAnyZone` once.
- **`container` scope** → align each container independently but sharing zone headroom. Run
  the check per container on a scratch copy of the zones, subtracting the chosen zone's
  resources after each container (greedy, first-fit lowest zone). This matches the upstream
  `singleNUMAContainerLevelHandler`. Init containers run serially and are checked but not
  accumulated.

The predicate is **pure** (read-only); it never mutates `nodes`. It also runs only on nodes
that already passed the existing whole-node vector gate, so it is naturally late in the
funnel.

### In-cycle reservation (EventHandler)

Within-cycle correctness rides the existing session `EventHandler`
(`framework.Event{Task}`), which fires symmetrically on commit and on rollback/undo:

```
AllocateFunc(e):
    nt = nodes[e.Task.NodeName]
    if !shouldHandle(e.Task, nt): return
    z = pickZone(nt, requests(e.Task))        // same fit the predicate accepted
    decrement nt.zones[z] by requests(e.Task)
    reserved[e.Task.UID] = z.id

DeallocateFunc(e):
    z = reserved[e.Task.UID]; if none: return
    increment nt.zones[z] by requests(e.Task)
    delete reserved[e.Task.UID]
```

Because the statement's undo path fires `DeallocateFunc` on rollback (and `AllocateFunc` on
redo), preemption/reclaim scenario probing — which speculatively allocates and `Discard()`s —
stays consistent automatically, with **no manual clone/restore**. Recording the chosen zone
id (rather than recomputing it) guarantees the restore targets the exact zone even though
headroom changed in between. The chosen zone is internal accounting only; it is never sent to
the kubelet, which independently re-derives placement.

This layer covers placement *within one cycle* only; it does **not** persist (speculative
allocations from preemption probing must never leak into long-lived state). Across cycles, v1
relies on the kubelet as the backstop and accepts the staleness window discussed in
[Appendix A](#appendix-a-optional-cross-cycle-staleness-compensation).

### Policy evaluator seam

The admit / zone-selection decision is isolated behind a small interface so v1's proven-safe
path is untouched when faithful `restricted` ([v2](#v2-faithful-restricted-via-reimplemented-hint-merge))
is added:

```go
// evaluate returns whether the pod can be NUMA-aligned on this node, and the
// zone(s) the in-cycle reservation should charge — one zone for single-numa-node,
// potentially several for a faithful restricted merge.
type numaEvaluator interface {
    evaluate(nt *nodeTopology, req resourceRequests) (zones []*numaZone, admit bool)
}
```

v1 ships a single `singleNUMAEvaluator` (the bitmask intersection above), used for both
`single-numa-node` and the conservatively-mapped `restricted`. The predicate and the
`AllocateFunc`/`DeallocateFunc` reservation both route through `evaluate` (the reservation
charges the returned `zones`, which in v1 is always exactly one). Adding v2 is then a matter
of registering a second evaluator and routing `restricted` nodes to it.

### Registration

Register the builder in `pkg/scheduler/plugins/factory.go`:

```go
framework.RegisterPluginBuilder("numa", numa.New)
```

and enable it in the scheduler plugin configuration. The allowlist and the
`restricted`-handling mode are read from `PluginArguments`.

### Deployment guidance: NRT freshness vs. schedule period

The cross-cycle staleness window (see *Known Limitations*) is an **operational** concern
before it is a code concern. The recommended deployment closes it without any cross-cycle
state in the plugin:

- **Keep the exporter's event-driven updates enabled (the default).** Both exporters — NFD's
  topology-updater ([nfd-tu]) and the resource-topology-exporter (RTE, [rte]) — watch the kubelet
  state directory (`cpu_manager_state`, `memory_manager_state`, `kubelet_internal_checkpoint`)
  via fsnotify and republish NRT immediately on an allocation change, *in addition to* a periodic
  refresh (`-sleep-interval`/`--sleep-interval`, default **60s**, configurable to any duration or
  to `0` to disable periodic updates). So NRT is normally fresh within ~sub-second to a few
  seconds of a pod start/stop. Do **not** chase freshness by driving the *periodic* interval very
  low — that is a per-node-per-interval write storm at fleet scale; the **event** path is what
  delivers freshness. (RTE rate-limits event scans via `--max-events-per-second`, default 1.)
- **Raise `--schedule-period`** (default `1s`) to, e.g., `5s`. This gives the full
  bind → kubelet-admit → exporter → apiserver → informer pipeline time to reflect a binding
  before the next cycle, so prior binds are visible and the hot-loop does not form. Note this
  is a **global** knob — it raises scheduling latency for *all* pods, which is generally
  acceptable for AI/ML batch workloads but should be weighed for latency-sensitive ones.
- **Observe it.** Emit a metric/log when the kubelet rejects a NUMA pod
  (`TopologyAffinityError`) or when the scheduler re-selects a node it just failed on. This
  reveals whether the timing assumption actually holds in a given fleet — and therefore whether
  [Appendix A](#appendix-a-optional-cross-cycle-staleness-compensation) is ever needed.

This is a timing assumption, not a guarantee: under bind bursts, kubelet admission lag, or
exporter backlog the window can still exceed a cycle. The kubelet preserves correctness
regardless; Appendix A is the in-plugin fallback if the assumption proves insufficient.

## Correctness and Known Limitations

- **The kubelet is the backstop.** Any divergence between this plugin and the kubelet costs
  extra reschedules, never correctness.
- **Provider-participation divergence.** NRT reports `cpu`/`memory` per zone even when the
  kubelet's CPU Manager is not `static` (in which case CPU is not actually hint-aligned).
  `single-numa-node` deployments almost always run CPU Manager `static` + Memory Manager, so
  the assumption holds in practice; documented as a divergence source.
- **Greedy container-scope packing** is order-sensitive and an approximation of the kubelet's
  per-container hint merge. Exact in the common single-GPU-container case.
- **Cross-cycle staleness is not compensated in code in v1.** Between binding a NUMA pod and the
  exporter republishing NRT (near-real-time when event-driven updates are active, else up to the
  periodic refresh, default ~60s), the scheduler may re-pick the same node off stale `Available`;
  under packing pressure this can produce a bounded reschedule hot-loop until NRT catches up. The
  kubelet still preserves correctness. The recommended mitigation is operational (keep
  event-driven updates on + optionally a longer `--schedule-period`, see *Deployment guidance*);
  [Appendix A](#appendix-a-optional-cross-cycle-staleness-compensation) is the in-plugin fallback.
- **Reclaim-simulation accuracy.** The scheduler never observes a pod's *actual* NUMA zone (NRT
  is aggregate per-zone only); it predicts it. So reclaim/preemption of NUMA pods is simulated
  on predicted victim zones and can occasionally waste an eviction when the pending pod needs
  multiple per-zone-scarce resources co-located (GPU-bound pods with abundant per-zone CPU are
  largely immune). **Until the optional [per-node NUMA placement agent](../numa-placement-agent/README.md)
  is implemented (Appendix B), reclaim predictions are not accurate** — they rely on
  prediction + the kubelet backstop. The worst case is a wasted eviction and a bounce, never a
  loop.
- **`restricted` is over-strict** by design (mapped to `single-numa-node`).

## Testing

- **Unit**: policy/scope parsing from NRT attributes (and legacy `TopologyPolicies`); the
  bitmask filter across single/multi-zone fits; QoS gating; allowlist intersection; pod- vs
  container-scope; `shouldHandle` rejection of fractional/MIG/non-Guaranteed pods.
- **Reservation**: in-cycle multi-pod placement on a multi-NUMA node; rollback consistency
  through allocate → discard (preemption probing).
- **E2E** (with a Kind node exposing synthetic NRT objects): a Guaranteed whole-GPU pod is
  filtered off a node whose free GPU/CPU cannot co-locate, and placed on one where they can.

## Future Work

- NUMA scoring (`AddNodeOrderFn`) to prefer least-fragmented placement.
- Cross-cycle staleness compensation if the hot-loop proves real in practice — see
  [Appendix A](#appendix-a-optional-cross-cycle-staleness-compensation).
- Fractional / MIG GPU support, if/when a meaningful kubelet alignment path exists.
- Faithful `restricted` (multi-NUMA) support — see
  [v2](#v2-faithful-restricted-via-reimplemented-hint-merge).

## v2: Faithful `restricted` via reimplemented hint merge

### Motivation and scope

`restricted` lets a pod span more than one NUMA node — but only when the alignment is the
*minimal* one possible. v1 conservatively maps `restricted` → `single-numa-node`, which
rejects any pod that genuinely needs ≥2 NUMA nodes. v2 reproduces the kubelet's admission
decision so those pods can be placed.

The value is **narrow**: as the examples below show, `restricted` admits a multi-NUMA pod
only when *every* topology-aware resource it requests independently needs the *same* minimal
set of NUMA nodes. So v2 only unlocks **large, balanced** pods (e.g. many GPUs **and** enough
CPU that both exceed single-node capacity). For the common "1 GPU + a little CPU + a NIC" pod
every resource needs one node, so `restricted` and `single-numa-node` admit the same set and
v2 adds nothing. Implement v2 only if large balanced multi-NUMA pods are real in the fleet.

### How the kubelet decides (the model v2 reproduces)

A **hint** is `{NUMANodeAffinity bitmask, Preferred bool}` — a candidate set of NUMA nodes a
hint provider (CPU Manager, Memory Manager, Device Manager) can satisfy its slice of the
request from. Each provider lists the NUMA-node subsets that can supply its requested amount
from per-zone availability, marking `Preferred=true` on those using the **minimum** number of
NUMA nodes the request physically needs. A hint is a candidate grouping, **not** an
allocation — it names no specific device or core.

The Topology Manager merges one hint per provider (cross-product). For each permutation
(`mergePermutation` in `k8s.io/kubernetes/.../topologymanager`):

- merged affinity = **bitwise-AND** of the picked affinities;
- merged is `Preferred` **iff all picked affinities are equal *and* all are individually
  preferred** (the kubelet's "only mark preferred if all affinities are equal" rule).

`restricted` admits **iff the best merged hint is `Preferred`**. Because the merge always
prefers a preferred hint when one exists, this reduces to a clean, short-circuitable rule:

> **`restricted` admits ⟺ there exists a NUMA-node mask `M` such that, for every
> topology-aware resource the pod requests, `M` is a preferred (minimal-width) satisfying
> hint for that resource.**

`single-numa-node` is the special case `|M| = 1`, so v2 generalizes v1 from "find one zone
that fits everything" to "find a common minimal-width mask." The full `compare` /
`BestNonPreferredAffinityCount` machinery in the kubelet only selects *which* non-preferred
hint wins for `best-effort`; it is not needed for the `restricted` admit decision.

On admission the kubelet stores `M`, and each provider then allocates **within** `M`, picking
specific devices/cores itself. The **per-zone split is not fixed at admission** — any
allocation drawing every resource from nodes in `M` is acceptable.

### Worked examples (node has 2 NUMA nodes)

| Per-node capacity | Pod (Guaranteed) | Per-resource preferred masks | Common mask? | `restricted` verdict |
| --- | --- | --- | --- | --- |
| 4 GPU, 16 CPU | 6 GPU + 10 CPU | GPU `{0,1}`; CPU `{0}`/`{1}` | none (GPU needs 2, CPU needs 1) | **reject** |
| 4 GPU, 16 CPU | 6 GPU + 24 CPU | GPU `{0,1}`; CPU `{0,1}` | `{0,1}` | **admit on `{0,1}`** |
| 2 GPU, many CPU | 4 GPU + 1 CPU | GPU `{0,1}`; CPU `{0}`/`{1}` | none | **reject** |

The third row is the instructive footgun: a 4-GPU pod that obviously *could* run 2+2 with its
single CPU placed anywhere is **rejected by the kubelet itself** under `restricted`, because
the CPU's minimal width (1) disagrees with the GPU's (2). The only ways to make it run are to
raise the CPU (or memory) request above a single node's capacity so its minimal width also
becomes 2, or to use `best-effort`. v2 faithfully reproduces this rejection — it does not (and
must not) "fix" it.

### Why reimplement rather than import

The valuable, intricate part — the merge + `Preferred`/admit rule — is small (the admit
short-circuit above is a few dozen lines). Per-resource hint generation (enumerate NUMA-node
subsets from per-zone `Available`, mark minimal-width preferred) is generic; there is **no
vendor-specific hint code** in the kubelet — device hints are generic, driven by per-device
NUMA affinity, which NRT already encodes as per-zone counts. Importing
`k8s.io/kubernetes/.../topologymanager` (an internal kubelet package) would couple KAI to
unstable kubelet internals across versions; notably, upstream scheduler-plugins imports only
`bitmask` and reimplements the rest. v2 follows that lead and reimplements the merge over the
plugin's per-zone model.

### Prior art: how others handle `restricted`

- **kubernetes-sigs/scheduler-plugins (NodeResourceTopology):** its Filter enforces only
  `single-numa-node`; for `restricted`/`best-effort` it passes through, leaving `restricted` to
  the kubelet. Its Score's per-zone strategies are likewise `single-numa-node`-only — a separate,
  policy-agnostic `LeastNUMANodes` strategy merely ranks nodes by NUMA span and is not
  `restricted`-specific. So it does **not** pre-compute the `restricted` verdict.
- **Volcano (`numa-aware` plugin):** *does* pre-compute `restricted` distinctly — it reads each
  node's Topology Manager policy from its own `Numatopology` CRD (published by a Volcano node
  agent), instantiates a dedicated restricted policy, runs a kubelet-style hint merge, and
  rejects the node when the best merged hint is not `Preferred`. Two caveats relevant to KAI: it
  reasons over **CPU hints only** (no GPU/device hint provider), and its merge is a *simplified*
  variant of the kubelet's (it drops the "all affinities equal" preferred rule and the
  `bestNonPreferredAffinityCount` tie-break), so its verdict can diverge from the real kubelet.

This validates v2's direction — reimplement the merge (the kubelet packages are not cleanly
importable) — while highlighting the gap KAI targets: **GPU/device** NUMA alignment, which
Volcano's CPU-only plugin does not cover. Volcano's per-pod placement tracking (`assignRes`) plus
its node agent is also close prior art for the
[placement agent](../numa-placement-agent/README.md).

### Implementation

- A `resourceHinter` registry (the "mini-plugin" mechanism): per allowlisted resource,
  generate `[]hint` from per-zone `Available`. One generic counting hinter covers
  `nvidia.com/gpu` and `memory`; `cpu` needs care (full physical cores / SMT) — see caveats.
- A `restrictedEvaluator` implementing `numaEvaluator`: build per-resource hints, search for a
  common minimal-width mask `M` (the admit short-circuit), return `(zonesOf(M), admit)`.
- Gate: route a `restricted` node to the `restrictedEvaluator` **only if every requested
  topology-aware resource has a registered hinter**; otherwise fall back to the v1
  conservative `singleNUMAEvaluator` (a safe, superset rejection).
- Reservation: for a multi-zone `M`, charge the zones with an **approximate greedy split**.

### v2 caveats

- **Reservation split is inherently loose.** The kubelet itself does not fix the per-zone
  split at admission (only the mask `M`), so the greedy charge approximates something left
  open upstream. It prevents gross within-cycle over-placement, but a later pod in the same
  cycle can still collide under packing pressure — a residual `TopologyAffinityError` risk
  that v1's exact single-zone charge never had.
- **CPU fidelity.** The count-based CPU hinter must match the CPU Manager's minimal width
  (full physical cores / SMT under the static policy). Since CPU's minimal width participates
  in the common-mask test, a divergence can flip an *admit* decision and cause a
  `TopologyAffinityError`. v2 is most trustworthy when the **GPU** drives the multi-NUMA span.
- **Loss of the safety guarantee.** Admitting multi-NUMA pods means divergence can cause a
  `TopologyAffinityError`; v1's conservative path provably cannot. The hinter-coverage gate
  preserves safety only for pods using unsupported resources, not supported-but-divergent ones.

## Appendix A: (optional) cross-cycle staleness compensation

**Status: optional, not part of v1, and the *second* line of defense.** First apply the
operational mitigation in *Deployment guidance* (event-driven NRT exporter + longer
`--schedule-period`), which closes the staleness window in the common case with no in-plugin
state. Implement this appendix only if the observability signal shows the bounded reschedule
hot-loop still matters in practice. It does not affect correctness (the kubelet is the
backstop) — only scheduling efficiency during the NRT refresh window.

### The problem

The schedule period is **1s** (`defaultSchedulerPeriod`). NRT is republished by the exporter
near-real-time on allocation changes (event-driven), but can lag up to its **periodic** refresh
(default 60s, configurable; see *Deployment guidance*) if event updates are disabled or delayed.
During any such lag, NRT `Available` still shows the pre-binding state: a second NUMA pod can be
placed on the same node off stale data; under packing pressure the kubelet rejects it
(`TopologyAffinityError`), and since the next cycle sees the same stale NRT the scheduler
re-picks the same node — a hot-loop until NRT catches up. With event-driven updates active this
window is small; this appendix matters only when it is not.

### The clean signal: the NRT pod fingerprint

The hard part of any cross-cycle cache is *eviction* — knowing when NRT has caught up so the
cache can stop compensating. There is a deterministic signal for this: the **pod fingerprint**
([`podfingerprint`](https://github.com/k8stopologyawareschedwg/podfingerprint)). The exporter
hashes the set of pods (by `namespace+name`) whose resources it accounted for when building the
NRT object and publishes it on the object:

- attribute **`nodeTopologyPodsFingerprint`**, with **`nodeTopologyPodsFingerprintMethod`** =
  `all` or `with-exclusive-resources` (legacy annotation `topology.node.k8s.io/fingerprint`).

It lets the scheduler answer *"does this NRT object already reflect the pods I know about?"*
exactly:

1. List the pods the scheduler sees on the node (it already exposes its own just-bound pods to
   the next snapshot). Use the **`with-exclusive-resources`** subset to match the exporter's
   method — that subset is exactly our Guaranteed whole-GPU pods.
2. Compute their fingerprint and compare to the NRT object's `nodeTopologyPodsFingerprint`.
3. **Match** → NRT accounts for exactly that pod set → it is current → trust `Available`.
   **Mismatch** → a pod the scheduler knows (e.g. a just-bound one) is not yet reflected → do
   not trust `Available` for this node.

This is the mechanism upstream's production NRT cache uses (`OverReserve.Resync`).

### Serving a dirty node: never skip — reconstruct

A first instinct is to **skip** a dirty node (fail the NUMA predicate on it until it goes
clean). Do **not**: it breaks multi-cycle reclaim. A reclaim/preempt decision spans cycles —
victims drain over their `terminationGracePeriod` while the pending pod is *pipelined* onto the
node. If the node disappears from NUMA consideration mid-drain, the solver re-plans onto a
different node and **evicts a second set of victims** while the first set is already dying.
Staleness would thus actively *multiply* evictions. The node must stay a candidate.

So a dirty node is served a **reconstructed** view rather than being dropped:

- **Clean node (fingerprint match):** use NRT `Available` directly — ground truth, already
  reflecting *every* pod on the node (ours or not). No prediction.
- **Dirty node (mismatch):** reconstruct per-zone availability from the snapshot,
  `available[zone] = capacity[zone] − Σ predicted_occupancy[zone]` over **all** NUMA pods on the
  node (`capacity` = static per-zone NRT `Allocatable`; each pod assigned a predicted zone via
  the evaluator). Used only while dirty; the next match reverts to NRT.

The fingerprint gate is what makes reconstruction safe: it is **transient**, so it cannot drift
permanently the way an *ungated* reconstruction would (one that never defers to ground truth and
strands capacity under fragmentation).

**No foreign-pod special case.** Upstream rejects nodes carrying pods it did not schedule,
because its cache is built only from its own `Reserve` calls and has no record of a foreign pod
to subtract. KAI's snapshot already contains *every* pod on the node (it must, for whole-node
`IdleVector`), and no pod's zone is ever *observed* anyway — ours included, all zones are
predictions. So reconstruction treats foreign and self-scheduled pods identically; there is
nothing special about a pod we did not place.

**Eviction credits the freed zone.** Because reconstruction assigns every live NUMA pod a
predicted zone, the in-cycle `DeallocateFunc` credits a victim's zone back when a reclaim
scenario (speculatively) evicts it — so NUMA-pod reclaim scenarios succeed without re-planning.
The prediction need only be **internally consistent**, not match the kubelet: a wrong victim
zone just means the pending pod is pipelined onto a zone label differing from where the kubelet
actually frees a GPU — but a GPU *did* free, so the kubelet still admits it. Mispredicted zones
cost internal precision, never correctness. The optional
[per-node NUMA placement agent](../numa-placement-agent/README.md) (Appendix B) removes the
prediction entirely by reporting each pod's *observed* zone, making reclaim simulation exact.

### Caveats and the no-fingerprint fallback

- **Requires a fingerprint-emitting exporter** (RTE publishes it; plain NFD topology-updater may
  not). No attribute → no clean/dirty signal → fall back to the operational mitigation, or the
  gap-bounded correction below.
- **v1 fingerprint is `namespace+name`, not UID** — aliases only if *naked* pods are recreated
  with the same name under churn; a non-issue with controllers (unique generated names).
- **Transient over-report during a dirty window.** Reconstruction predicts zones, so it can
  briefly over-report a zone and earn a kubelet rejection — bounded to the window and caught by
  the kubelet backstop. This is the accepted cost of keeping the node usable (vs. skip) for
  reclaim stability.
- **No fingerprint? Gap-bounded fallback.** Without the clean/dirty gate, anchor on NRT
  `Available` and subtract only up to the measured lag `gap = Σ_zone Available −
  KAI_exact_node_free` (KAI's whole-node free never lags); it auto-decays as NRT catches up.
  The drift warning applies specifically to *ungated* reconstruction — with no fingerprint to
  snap back to NRT, predicting all pods' zones every cycle never defers to ground truth.

## Appendix B: (optional) per-node NUMA placement agent

**Status: optional, not part of v1.** Full design:
[Per-Node NUMA Placement Agent](../numa-placement-agent/README.md).

The scheduler never observes a pod's actual NUMA zone — NRT is aggregate per-zone only — so the
plugin *predicts* placement. Prediction is fine for filtering (the kubelet backstops admission)
but makes reclaim simulation inexact: **until this agent exists, reclaim predictions for NUMA
pods are not accurate** (they rely on predicted victim zones + the kubelet backstop; worst case
is a wasted eviction, see *Reclaim-simulation accuracy* in Known Limitations).

The agent is a per-node DaemonSet that reads the kubelet **podresources API**, derives each
pod's actual per-zone resource placement, and publishes it as a pod annotation
(`kai.scheduler/numa-placement`). When present, the plugin uses *observed* placement instead of
predicting it: per-zone occupancy becomes exact, victim evictions credit the real zone, and
reclaim simulation is accurate. When absent, the plugin falls back to prediction — so the agent
is purely additive and can be enabled independently, after v1.

[nrt-api]: https://github.com/k8stopologyawareschedwg/noderesourcetopology-api
[nfd-tu]: https://github.com/kubernetes-sigs/node-feature-discovery/blob/master/pkg/nfd-topology-updater/kubeletnotifier/kubeletnotifier.go
[rte]: https://github.com/k8stopologyawareschedwg/resource-topology-exporter/blob/main/pkg/notification/notification.go
