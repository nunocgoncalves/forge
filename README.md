# forge

`forge` is the installer for the Horizonshift / Iterabase platform. It bootstraps a production-ready single-node [k3s](https://k3s.io) cluster on a VM or host over SSH, with dual-stack networking and prod-ready defaults.

> Per-customer, fully isolated, self-hosted. forge takes VMs/hosts (SSH) or a kubeconfig; it does **not** provision bare metal, Proxmox, or network appliances.

## Status

Walking skeleton (HOR-238). Currently implements single-node k3s bootstrap. The platform Helm umbrella chart, GPU/vLLM backend, Flux, and the overlay repo are deferred to follow-on tickets (HOR-239, HOR-240).

## Build

```sh
make build      # -> bin/forge
make lint
make test
```

## Quickstart

```sh
forge init               # generate forge.yaml (interactive)
forge apply --dry-run    # preflight the target host and print the plan (read-only)
forge apply              # provision / reconcile the cluster
forge kubeconfig         # fetch the kubeconfig
forge status             # cluster health + drift
```

See `forge.example.yaml` for the full substrate config schema.

## Layout

- `cmd/forge/` — entrypoint
- `internal/cli/` — Cobra command tree
- `internal/config/` — `forge.yaml` schema + loader
- `internal/provisioner/` — provisioner interface (the testability seam)
- `internal/sshprovisioner/` — SSH implementation
- `internal/k3s/` — k3s install-arg builder
- `internal/kubeconfig/` — kubeconfig fetch + rewrite
- `internal/lifecycle/` — phase orchestration + reconcile
- `internal/artifacts/` — local state dir (`~/.forge/<install>/`)
- `internal/version/` — build version
- `test/e2e/` — DigitalOcean cloud-VM e2e

## License

Proprietary — Horizonshift. All rights reserved.
