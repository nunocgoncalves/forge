# Forge E2E runner design

Decision: HOR-406, approved 2026-08-03.

## Goals

- Expose one Go test entrypoint and one GitHub Actions workflow.
- Compose scenarios from named stages and shared fixtures.
- Preserve every existing boundary assertion while avoiding redundant cloud setup.
- Keep isolation where clean installation is part of the contract.

## Runner

`TestE2E` is the only top-level infrastructure test. It registers named scenarios through `internal/runner`; `go test -run` selects a scenario and each scenario reports named stage subtests. Stages share explicit scenario state and stop after a failed prerequisite, while parent cleanup and diagnostics still run.

## Isolation boundary

### DigitalOcean CPU — one VM

Stages:

1. Provision a cloud-init-complete CPU host.
2. Assert `gpu.enabled` is rejected when no NVIDIA GPU exists.
3. Apply the baseline k3s + chart + local edge overlay.
4. Assert node readiness, dual-stack pod CIDRs, label propagation, gateway pod readiness, and HTTPS/MetalLB health.
5. Re-apply and assert the reality-derived action is `skip` while configured phases reconcile successfully.
6. Apply the public overlay tokenlessly and assert commit/clone/chart/CRD mechanics.
7. Materialize an overlay-declared Secret from an operator environment variable and verify its value/type.
8. Install Flux and assert controllers, source artifact, and Kustomization readiness.

Only baseline needs a fresh host. Overlay, secret-sync, and Flux are phases on a ready forge substrate; separate VMs repeated k3s installation without testing another boundary. The composed flow also exercises real re-entry between configs.

### DigitalOcean GPU — one VM

Stages:

1. Apply k3s + GPU operator without the platform chart.
2. Assert ClusterPolicy readiness and run a real GPU smoke pod.
3. Reconcile the platform chart on the same host with the already-proven GPU phase skipped.
4. Apply identity/catalog resources, wait for real vLLM availability, assert rendered extra arguments, and request a real completion.

The inference path depends on GPU readiness, so a second VM/operator installation added cost and capacity noise rather than isolation. The chart is introduced only after the substrate smoke assertion, preserving fault localization.

### Kind — fresh cluster per contract

- `kind-controlplane-identity`: standalone control-plane chart and identity/JWT contract.
- `kind-inference-contract`: umbrella cross-service catalog/auth contract.
- `kind-cert-issuers`: minimal cert-manager/self-signed issuer values contract.
- `kind-internal-tls`: two-phase Helm transition and live internal transport contract.
- `kind-tool-runner-contract`: exact Flux artifact materialization through the chart-managed Node runner, mTLS gateway registration, and pinned generation drain using coordinated local control-plane/charts builds.

These remain isolated because clean chart installation, cluster-scoped CRDs/issuers, hooks, and value combinations are part of what they test. Sharing a cluster could mask missing resources or leak state between releases.

## Coverage mapping

| Previous test | New scenario/stage | Coverage |
|---|---|---|
| `TestE2E` | `digitalocean-cpu/apply-baseline`, `assert-baseline` | k3s, chart, overlay, node, dual-stack, label, gateway edge |
| `TestGPUE2E_PreflightFail` | `digitalocean-cpu/reject-gpu-on-cpu-host` | no-GPU preflight refusal; now actually selected by CI |
| `TestE2EOverlay` | `digitalocean-cpu/apply-public-overlay` | public tokenless clone, commit, values, CRD path |
| `TestE2ESecrets` | `digitalocean-cpu/sync-secrets` | env → SSH stdin → Kubernetes Secret |
| `TestE2EFlux` | `digitalocean-cpu/install-and-reconcile-flux` | controllers, GitRepository artifact, Kustomization |
| `TestGPUE2E` | `digitalocean-gpu/apply-gpu-substrate`, `assert-gpu-smoke` | operator readiness and usable GPU |
| `TestInferenceFlowGPU` | `digitalocean-gpu/apply-platform`, `run-real-inference` | real control-plane/vLLM/gateway completion |
| `TestControlPlaneIdentity` | `kind-controlplane-identity` | unchanged, fresh Kind cluster |
| `TestInferenceFlowContract` | `kind-inference-contract` | unchanged plus restored PermissionPolicy materialization |
| `TestCertIssuers` | `kind-cert-issuers` | unchanged, fresh Kind cluster |
| `TestInternalTLS` | `kind-internal-tls` | unchanged, fresh Kind cluster |
| HOR-397 cross-repository acceptance | `kind-tool-runner-contract` | Flux artifact → materializer → runner → mTLS registration → pinned drain |

## CI

One `e2e.yml` workflow owns:

- fast harness compilation, unit tests, and nested-module lint;
- one serialized CPU cloud job;
- one serialized GPU cloud job;
- a fail-fast-disabled Kind matrix with one fresh runner/cluster per contract; the tool-runner contract checks out coordinated control-plane and chart sources and builds their images locally.

All E2E invocations are verbose so capacity skips and stage results are visible. Cloud jobs never cancel in progress, allowing test cleanup to destroy VMs; the tagged reaper remains the crash safety net.

## Rejected alternatives

- **Merged mega-suite/shared Kind cluster:** weaker artifact-boundary signal and cluster-scoped state leakage.
- **Fresh VM for every forge phase:** repeated provisioning and k3s/GPU installation without an independent requirement.
- **Cross-repository harness library:** outside HOR-406's forge-only scope and premature before another repository demonstrates the same fixture contract.

## Production impact

None. This changes test infrastructure only. `FORGE_E2E_KEEP` retains either cloud fixture for debugging; normal cleanup and the reaper remain in place.
