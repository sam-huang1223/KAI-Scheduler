# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Changed
- Updated Go toolchain and base build images to v1.25.10.

## [v0.6.20] - 2026-05-07

### Fixed
- Improved reclaim, preempt, and consolidation performance by skipping solver work for jobs blocked by victim-invariant pre-predicate failures such as missing PVCs, missing required ConfigMaps, and tasks larger than any node. [#1502](https://github.com/kai-scheduler/KAI-Scheduler/issues/1502)
- Fixed post-delete cleanup hook hardcoding `kai-scheduler` namespace instead of Helm release namespace on `helm uninstall` [#1619](https://github.com/kai-scheduler/KAI-Scheduler/pull/1619) [dttung2905](https://github.com/dttung2905)

## [v0.15.0] - 2026-05-20

### Added
- Added `enabled` Helm values for `binder`, `podgrouper`, `podgroupcontroller`, `queuecontroller`, `admission`, and `scheduler` to allow disabling individual components from values.yaml. Previously these were hardcoded to `true` in the kai-config template.
- Added `prometheus.enabled` and `prometheus.externalPrometheusUrl` Helm values to configure Prometheus from values.yaml [#907](https://github.com/NVIDIA/KAI-Scheduler/issues/907)
- Added validation for `subgroup` name in podgroup [faizanexe](https://github.com/faizan-exe)
- Added memory profile and run duration to snapshot tool [#1411](https://github.com/NVIDIA/KAI-Scheduler/issues/1411)
- Added support for configuring pod and container security contexts on resource reservation pods via CLI flags [AdheipSingh](https://github.com/AdheipSingh)
- Added `operator.logLevel` Helm value to configure the operator log level (maps to `--zap-log-level` when set) [#1446](https://github.com/kai-scheduler/KAI-Scheduler/pull/1446) [dttung2905](https://github.com/dttung2905)
- The scheduler now implements elastic PodGroups on both the subgroup level (`minSubGroup`) and pods (`minAvailable`). This allows for elasticity on all of the podgroup tree hierarchy. [#1416](https://github.com/kai-scheduler/KAI-Scheduler/pull/1416) - [davidLif](https://github.com/davidLif)
- Allow the configuration of plugins in the binder service. [#1480](https://github.com/kai-scheduler/KAI-Scheduler/pull/1480) - [davidLif](https://github.com/davidLif)
- Added support for configuring scheduler log level and custom scheduler args via Helm values (`scheduler.args`) [#1452](https://github.com/kai-scheduler/KAI-Scheduler/pull/1452) [dttung2905](https://github.com/dttung2905)
- Added `global.jsonLog` Helm value to enable JSON-formatted logging for use with log aggregation platforms
- Added `crdupgrader.image.registry` Helm value to override `global.registry` for the `crd-upgrader` pre-install/pre-upgrade hook image, allowing the hook image to be served from a separate mirror without redirecting all chart images. [#1404](https://github.com/kai-scheduler/KAI-Scheduler/issues/1404)
- Added support for externally-created PodGroups. Workloads can opt out of podgrouper mutation with `kai.scheduler/skip-podgrouper: "true"` on the pod or owner chain, join an existing PodGroup via `pod-group-name`, and now get a pod condition when they reference a non-existent subgroup. [#1420](https://github.com/kai-scheduler/KAI-Scheduler/issues/1420)
- Added `--stuck-in-releasing-threshold` scheduler flag (default `2m`) controlling how long a Running pod with a `deletionTimestamp` remains classified as `Releasing` before being reclassified as `StuckInReleasing` and excluded from pipelining. Configurable per shard via `SchedulingShard.spec.args.stuck-in-releasing-threshold`.

### Changed
- **Breaking:** JobSet PodGroups no longer auto-calculate `minAvailable` from `parallelism × replicas`. The default is now 1. Use the `kai.scheduler/batch-min-member` annotation to set a custom value.
- Bumped `k8s.io/*` module group from v0.34.x to v0.35.4, `k8s.io/kubernetes` to v1.35.4, and `sigs.k8s.io/controller-runtime` to v0.23.3, enabling KEP-4671 Workload API types. [#1466](https://github.com/kai-scheduler/KAI-Scheduler/issues/1466)
- Rebuilt the `crd-upgrader` hook image on `alpine:3.20` instead of `ubi9/ubi-minimal`. Image size drops from ~165 MB to ~67 MB uncompressed (~60% reduction), shrinking cold-pull latency on ephemeral CI runners. The image is also reused by the `topology-migration` and `post-delete` hook jobs as a generic `kubectl + bash` toolbox, so bash is preserved on the runtime image. [#1404](https://github.com/kai-scheduler/KAI-Scheduler/issues/1404)

### Fixed
- Account for native sidecar containers (initContainers with `restartPolicy: Always`, KEP-753) in pod resource accounting, matching kubelet's `AggregateContainerRequests`. Previously, native sidecar requests were max'd against regular containers instead of summed with them, causing the scheduler to bind pods that kubelet then rejected at admission with `OutOfCpu`/`OutOfGpu`. [#1556](https://github.com/kai-scheduler/KAI-Scheduler/pull/1556)
- Streaming snapshot JSON directly into the zip writer to avoid OOM on large clusters. The `/get-snapshot` endpoint previously buffered the entire JSON payload in memory (~3x the data size); it now streams per-element, reducing peak memory to ~1x. [#1564](https://github.com/kai-scheduler/KAI-Scheduler/pull/1564)
- Fixed `additionalImagePullSecrets` in Config CR rendering as `map[name:...]` instead of plain strings by extracting `.name` from `global.imagePullSecrets` objects. Also propagated `global.imagePullSecrets` to all Helm hook jobs (`crd-upgrader`, `topology-migration`, `post-delete-cleanup`)
- Added `global.nodeSelector`, `global.tolerations`, `global.affinity`, `global.securityContext` support to the post-delete job hook.
- Fixed Helm template writing `imagesPullSecret` (string) instead of `additionalImagePullSecrets` (array) in Config CR, causing image pull secrets to be silently ignored. Added backward-compatible deprecated `imagesPullSecret` field to CRD schema. [#942](https://github.com/kai-scheduler/KAI-Scheduler/issues/942)
- Fixed `windowSize` field in `SchedulingShard` CR to support Prometheus duration format (e.g. `1w`, `7d`). Previously, using `windowSize: 1w` as shown in the documentation caused the kai-operator to crash-loop with `time: unknown unit "w" in duration "1w"`.
- Race condition where `SyncForGpuGroup` could prematurely delete reservation pods when the informer cache had not yet propagated GPU group labels on recently-bound fraction pods. The binder now checks for active BindRequests referencing the GPU group before deleting a reservation pod.
- Fixed non-preemptible multi-device GPU memory jobs being allowed to exceed their queue's deserved GPU quota. The per-node quota check now correctly accounts for all requested GPU devices. [#1369](https://github.com/kai-scheduler/KAI-Scheduler/issues/1369)
- Added `resourceclaims/binding` RBAC permission to the binder ClusterRole for compatibility with Kubernetes v1.36+, where the `DRAResourceClaimGranularStatusAuthorization` feature gate requires explicit permission on the `resourceclaims/binding` subresource to modify `status.allocation` and `status.reservedFor` on ResourceClaims. [#1372](https://github.com/kai-scheduler/KAI-Scheduler/pull/1372) [praveen0raj](https://github.com/praveen0raj)
- Allow users to override minMember for k8s batch Jobs and JobSets using the `kai.scheduler/batch-min-member` annotation [#1308](https://github.com/kai-scheduler/KAI-Scheduler/pull/1308) [itsomri](https://github.com/itsomri)
- Fixed a bug where nil minMember caused subgroups creation to fail in scheduler [#1407](https://github.com/kai-scheduler/KAI-Scheduler/pull/1407) [itsomri](https://github.com/itsomri)
- Improved performance by evaluating SetNode once per session instead of on each predicate evaluation  [#1421](https://github.com/kai-scheduler/KAI-Scheduler/pull/1421) [itsomri](https://github.com/itsomri)
- Added persistent volumes to cluster snapshot [#1424](https://github.com/kai-scheduler/KAI-Scheduler/pull/1424) [itsomri](https://github.com/itsomri)
- Improved scheduling performance for preempt/reclaim/consolidate actions on jobs with many tasks by replacing per-task linear probing with exponential+binary search in the job solver, reducing the number of scenario simulations from O(n) to O(log n) [#1435](https://github.com/kai-scheduler/KAI-Scheduler/pull/1435) [itsomri](https://github.com/itsomri)
- Avoid expensive solver-backed reclaim/preempt/consolidation work for jobs already blocked by victim-invariant pre-solver failures such as missing PVCs, missing required ConfigMaps, or requests larger than the maximum node size. [#1502](https://github.com/kai-scheduler/KAI-Scheduler/issues/1502)
- Fixed `skipTopOwnerGrouper` not propagating per-type defaults (priority class and preemptibility) for skipped owners (e.g. `DynamoGraphDeployment`), causing PodGroup spec to retain stale values after defaults ConfigMap updates.
- Fixed binder DRA detection on clusters where the upstream `DynamicResourceAllocation` feature gate does not reflect server-side DRA availability. The binder now probes the API server during init (matching the scheduler) so the DRA plugin is gated on the same authoritative decision. [#1481](https://github.com/kai-scheduler/KAI-Scheduler/issues/1481)
- Suppressed noisy `Reconciler error` logs and `PodGrouperWarning` events on transient PodGroup update conflicts. The podgrouper now treats `IsConflict` errors as expected and silently requeues the reconcile instead of surfacing the apiserver's "object has been modified" message.

## [v0.6.19] - 2026-04-29

### Fixed
- Fixed non-preemptible multi-device GPU memory jobs being allowed to exceed their queue's deserved GPU quota. The per-node quota check now correctly accounts for all requested GPU devices. [#1369](https://github.com/kai-scheduler/KAI-Scheduler/issues/1369)
- Race condition where `SyncForGpuGroup` could prematurely delete reservation pods when the informer cache had not yet propagated GPU group labels on recently-bound fraction pods. The binder now checks for active BindRequests referencing the GPU group before deleting a reservation pod.

## [v0.6.18] - 2026-03-24

### Fixed
- fixed podGroup status update loop on conflict[#1313](https://github.com/kai-scheduler/KAI-Scheduler/pull/1313)

## [v0.6.17] - 2026-01-26

### Changed

- Update go version to v1.25.6, with appropriate upgrades to the base docker images, linter, and controller generator. [#1281](https://github.com/kai-scheduler/KAI-Scheduler/pull/1281) [davidLif](https://github.com/davidLif)

### Fixed

- Updated resource enumeration logic to exclude resources with count of 0. [#1120](https://github.com/NVIDIA/KAI-Scheduler/issues/1120)
- Fixed plugin server (snapshot and job-order endpoints) listening on all interfaces by binding to localhost only.

## [v0.6.16] - 2026-01-07

### Fixed
- Fixed a bug where the scheduler would not re-try updating podgroup status after failure
- GPU Memory pods are not reclaimed or consolidated correctly
- Fixed GPU memory pods Fair Share and Queue Order calculations

## [v0.6.14] - 2025-08-26

### Removed
- Removed unused code that required gpu-operator as a dependency

### Fixed
- Fixed scheduler panic in some elastic reclaim scenarios
- Fixed wrong GPU memory unit conversion from node `nvidia.com/gpu.memory` labels
- Fixed incorrect MIG GPU usage calculation leading to wrong scheduling decision

## [v0.6.11] - 2025-08-18

### Changed
- Scheduler now allows reclaim scenarios when both queues are above fair share, if starvation ratio will improve

### Fixed
- kai-scheduler will not ignore pod spec.overhead field

## [v0.6.9] - 2025-07-18

### Fixed
- Fixed a scenario where only GPU resources where checked for job and node, causing it to be bound instead of being pipelined

## [v0.6.8] - 2025-07-13

### Fixed
- Fixed a miscalculation where cpu/memory releasing resources were considered idle when requesting GPU fraction/memory

## [v0.6.7] - 2025-07-07

### Added
- Added LeaderWorkerSet support in the podGrouper. Each replica will be given a separate podGroup.

## [v0.6.6] - 2025-07-06

### Fixes
- Fixed cases where reclaim validation operated on outdated info, allowing invalid reclaim scenarios

## [v0.6.4] - 2025-06-27

### Fixes
- Fix: pod group controller fails on missing priority class

## [v0.6.0] - 2025-06-16

### Changed
- Changed `runai-reservation` namespace to `kai-resource-reservation`. For migration guide, refer to this [doc](docs/migrationguides/README.md)
- Changed `runai/queue` label key to `kai.scheduler/queue`. For migration guide, refer to [doc](docs/migrationguides/README.md)

### Fixes
- Fixed pod status scheduled race condition between the scheduler and the pod binding
- Removed redundant `replicas` key for binder from `values.yaml` as it is not used and not supported

### Removed
- Removed `runai-job-id` and `runai/job-id` annotations from pods and podgroups

### Added
- Added [minruntime](docs/plugins/minruntime.md) plugin, allowing PodGroups to run for a configurable amount of time without being reclaimed/preempted.
- PodGroup Controller that will update podgroups statuses with allocation data.
- Queue Controller that will update queues statuses with allocation data.


## [v0.5.1] - 2025-05-20

### Added
- Added support for [k8s pod scheduling gates](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/)
- nodeSelector, affinity and tolerations configurable with global value definitions
- Added `PreemptMinRuntime` and `ReclaimMinRuntime` properties to queue CRD
- Scheduler now adds a "LastStartTimestamp" to podgroup on allocation

### Changed
- Queue order function now takes into account potential victims, resulting in better reclaim scenarios.

### Fixes
- Fixed preempt/reclaim of elastic workloads only taking one pod.
- Scheduler now doesn't label pods' nodepool when nodepool label value is empty
