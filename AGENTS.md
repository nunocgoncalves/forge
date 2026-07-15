# Project operating instructions

## Git and ticket workflow

- Direct pushes to `master` are prohibited.
- Each Linear ticket must be scoped to its own branch.
- Branch names, commit messages, and pull request titles must include the Linear ticket identifier, for example `HOR-123-short-description`, `HOR-123 describe change`, and `HOR-123 — Describe change`.
- Commit to the ticket branch as work progresses and as commits make sense.
- When work is ready for review, open a pull request; do not merge it yourself.
- After pushing a branch or opening a PR, watch the CI/CD workflows to completion (`gh run watch <run-id>` or the Actions tab) and stay in the iteration loop: if a workflow fails, triage and push a fix promptly. The work is not handed off until CI is green or the failure is understood.
- Pull request descriptions must be valid Markdown with real line breaks, not escaped `\n` text; when using `gh`, write the body to a file and use `--body-file` for both create/edit operations.
- Pull request descriptions should use this structure: `## Summary`, `## Validation`, `## Production impact`, and `## Ticket state`; include concise bullets under each heading and mark non-applicable sections as `None` or `N/A`.
- Only the user may approve and merge pull requests to `master`.
- A ticket is not complete until its branch has been merged to `master` and any required external checks have passed.
- The repository is the source of truth for non-secret infrastructure intent and architecture.
- Linear is the source of truth for ticket state, ownership, sequencing, and completion status.

## forge development

`forge` is a Go CLI (module `github.com/nunocgoncalves/forge`, Go 1.26) that bootstraps single-node k3s on a VM/host over SSH. Platform direction: see the Horizonshift Platform Direction note (Obsidian).

### Commands

```sh
make build          # -> bin/forge
make test           # unit + fake-SSH integration tests
make test-e2e       # DigitalOcean cloud-VM e2e (needs DIGITALOCEAN_TOKEN)
make lint           # golangci-lint
make fmt-check      # gofmt check
make install-hooks  # wire .githooks/ via core.hooksPath
```

### Architecture invariants (do not violate)

- **`Provisioner` interface seam.** Lifecycle/reconcile logic in `internal/lifecycle` orchestrates against `internal/provisioner.Provisioner`. The real SSH impl is `internal/sshprovisioner`. Logic must never call SSH directly — keep it behind the interface so it is unit-testable with fakes.
- **No shell-out.** forge talks to hosts via `golang.org/x/crypto/ssh` (in-process) and verifies via the remote host's bundled `k3s kubectl` over SSH. The operator machine needs no `ssh`/`kubectl`/`helm` installed. The e2e module (`test/e2e`, a separate Go module) may use `client-go` as a test dependency.
- **Reality-as-state.** `forge apply` reconciles from the live system (reads k3s version + config via SSH), not from a persisted state file. Local `~/.forge/<install>/` holds only operational artifacts (kubeconfig + audit.jsonl), re-fetchable from the host. There is no authoritative state file to lose.
- **Idempotency rules.** `apply` is safe to re-run: install if absent, skip if in sync, refuse immutable field changes (cluster-cidr/service-cidr/dualStack → `destroy` + `apply`), and route k3s version changes to `forge upgrade`.
- **Substrate vs overlay.** `forge.yaml` is the substrate recipe (hosts + k3s + node labels); the overlay repo (chart values + CRDs) is the runtime GitOps source of truth and is deferred to later tickets. Do not add chart-value/dep toggles to `forge.yaml` — those belong in the overlay.

### v1 boundaries (deferred to later tickets)

- Helm umbrella chart + Postgres/Redis/MinIO/services/ingress + Flux + overlay repo = HOR-239.
- GPU driver/toolkit/RuntimeClass (node readiness) = HOR-240; vLLM/SGLang workload deployment = HOR-306 (control-plane, CRD-driven from the overlay).
- Node labels/taints are applied at install via k3s flags (verbatim from config); label-drift reconciliation via the API is a fast-follow.
- Homebrew tap is deferred (goreleaser deprecated `brews`).
