// Package fluxer defines the host-level Flux GitOps toolkit interface — the
// testability seam for installing/uninstalling Flux (the flux CLI runs on the
// host over SSH, sharing the same transport as the k3s Provisioner, the Helm
// Deployer, and the overlay Overlayer). The real implementation lives in
// internal/sshprovisioner; tests use fakes. Lifecycle logic never invokes the
// flux CLI directly — it orchestrates against this interface.
package fluxer

import "context"

// Fluxer abstracts host-level Flux GitOps toolkit operations. One instance is
// bound to the same host as the Provisioner/Deployer/Overlayer; the flux CLI
// runs there over SSH against the k3s kubeconfig.
type Fluxer interface {
	// EnsureFlux installs the flux CLI on the host if absent (the official
	// version-pinned install script, mirroring ensureHelm/EnsureGit), then runs
	// `flux install` to apply the Flux components (source-controller,
	// kustomize-controller, helm-controller, notification-controller) + their
	// CRDs into the cluster. Idempotent: re-running reconciles to the version.
	// version is the flux2 release tag (e.g. "v2.4.0").
	EnsureFlux(ctx context.Context, version string) error
	// UninstallFlux runs `flux uninstall` (non-interactive) to remove the Flux
	// components + CRDs + flux-system resources. Best-effort (destroy): a missing
	// Flux install or absent flux CLI is not an error so destroy always proceeds
	// to substrate removal.
	UninstallFlux(ctx context.Context) error
	// GitRepositoryStatus reads the Ready condition of the forge-applied
	// GitRepository (informational; never gates apply — Flux reconciles async).
	// Returns ("", nil) when the CR/source is not yet present or on a transient
	// error, so a best-effort status read never fails the apply.
	GitRepositoryStatus(ctx context.Context, name string) (string, error)
}
