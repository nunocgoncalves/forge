// Package fluxer defines the host-level Flux GitOps toolkit interface — the
// testability seam for installing/uninstalling Flux (the flux CLI runs on the
// host over SSH, sharing the same transport as the k3s Provisioner, the Helm
// Deployer, and the overlay Overlayer). The real implementation lives in
// internal/sshprovisioner; tests use fakes. Lifecycle logic never invokes the
// flux CLI directly — it orchestrates against this interface.
package fluxer

import "context"

// GitRepositoryArtifact is the exact source-controller artifact state Forge
// gates on before starting workloads that consume the artifact server.
type GitRepositoryArtifact struct {
	Ready    bool
	Revision string
	Digest   string
}

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
	// GitRepositoryArtifact reads the Ready condition and exact revision/digest
	// of the forge-applied source. A missing/not-yet-ready source returns a zero
	// value so lifecycle polling can retry without treating convergence as an
	// API failure.
	GitRepositoryArtifact(ctx context.Context, name string) (GitRepositoryArtifact, error)
}
