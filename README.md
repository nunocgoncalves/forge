# forge

`forge` is the installer for the Horizonshift / Iterabase platform. It bootstraps a production-ready single-node [k3s](https://k3s.io) cluster on a VM or host over SSH, with dual-stack networking and prod-ready defaults.

> Per-customer, fully isolated, self-hosted. forge takes VMs/hosts (SSH) or a kubeconfig; it does **not** provision bare metal, Proxmox, or network appliances.

## Status

Walking skeleton (HOR-238). Implements single-node k3s bootstrap + the `forge` CLI. The platform Helm umbrella chart, GPU/vLLM backend, Flux, and the overlay repo are deferred to follow-on tickets (HOR-239, HOR-240).

## Install

Pre-built binaries are published on the [GitHub Releases](https://github.com/nunocgoncalves/forge/releases) page (linux/darwin × amd64/arm64), with checksums and an SBOM.

> Homebrew tap: deferred — `goreleaser` deprecated its `brews` section; the tap will return once the replacement stabilizes.

Build from source:

```sh
make build      # -> bin/forge
```

## Quickstart

```sh
forge init               # generate forge.yaml (interactive)
forge apply --dry-run    # preflight the target host and print the plan (read-only)
forge apply              # provision / reconcile the cluster
forge kubeconfig         # fetch (or refresh) the kubeconfig
forge status             # cluster health + drift
forge upgrade --to v1.32.0   # upgrade k3s
forge destroy            # uninstall k3s + remove local artifacts
```

`forge` SSHes to the host as a sudoer user (key auth) and installs k3s with flags derived from `forge.yaml`. The kubeconfig is fetched, rewritten to the host address, and stored at `~/.forge/<install>/kubeconfig.yaml`.

`apply` is **idempotent**: it reconciles from the live system — installs if absent, skips if in sync, refuses immutable changes (`cluster-cidr`/`service-cidr`/`dualStack` → `destroy` + `apply`), and routes version changes to `upgrade`.

See `forge.example.yaml` for the full substrate config schema.

## Development

```sh
make test           # unit + fake-SSH integration tests
make test-e2e       # DigitalOcean cloud-VM e2e (needs DIGITALOCEAN_TOKEN)
make lint           # golangci-lint
make fmt-check      # gofmt check
make install-hooks  # wire .githooks/ via core.hooksPath
```

Architecture invariants and v1 boundaries are documented in `AGENTS.md`.

## Layout

- `cmd/forge/` — entrypoint
- `internal/cli/` — Cobra command tree
- `internal/config/` — `forge.yaml` schema + loader
- `internal/provisioner/` — provisioner interface (the testability seam)
- `internal/sshprovisioner/` — SSH implementation
- `internal/k3s/` — k3s install-arg builder
- `internal/kubeconfig/` — kubeconfig fetch + server-URL rewrite
- `internal/lifecycle/` — phase orchestration + reconcile
- `internal/artifacts/` — local state dir (`~/.forge/<install>/`)
- `internal/version/` — build version
- `test/e2e/` — DigitalOcean cloud-VM e2e (separate module)

## License

Proprietary — Horizonshift. All rights reserved.
