// Package deployer defines the cluster-level manifest operations interface
// (Helm + kustomize) — the testability seam for installing/upgrading/uninstalling
// Helm releases (the iterabase-platform chart, the NVIDIA GPU Operator, etc.) and
// applying kustomize directories (overlay CRD instances). The real implementation
// lives in internal/sshprovisioner (helm + k3s kubectl run on the host over SSH,
// sharing the same transport as the k3s Provisioner); tests use fakes. Lifecycle
// logic never talks to SSH, helm, or kubectl directly.
package deployer

import "context"

// ChartState is the reconciled state of the platform chart release, read for
// status reporting. A release that does not exist is {Installed: false}.
type ChartState struct {
	Installed bool   // a helm release exists for this name/namespace
	Status    string // helm status: deployed/failed/pending-upgrade/pending-install/...
	Version   string // installed chart version (semver)
}

// ApplyOpts configures a Helm release install/upgrade (helm upgrade --install).
type ApplyOpts struct {
	Release    string   // helm release name
	Repository string   // chart ref (OCI URL, e.g. oci://.../iterabase-platform, or repo/name)
	Version    string   // chart version (semver)
	Namespace  string   // target namespace (--create-namespace)
	Values     []string // --set inline values (e.g. GPU operator overrides)
	ValueFiles []string // -f value files, applied in order (later wins); overlay values
}

// Deployer abstracts cluster-level manifest operations (Helm + kustomize). One
// instance is bound to a single host (the same host the Provisioner bootstrapped
// k3s on); helm + k3s kubectl run there over SSH using the k3s kubeconfig.
type Deployer interface {
	// Apply idempotently installs or upgrades a Helm release (helm upgrade
	// --install), applying -f value files (ValueFiles, in order) then --set
	// values (Values). It ensures helm is present first.
	Apply(ctx context.Context, opts ApplyOpts) error
	// EnsureRepo adds (or force-updates) a Helm repository on the host. Needed
	// for repo-based charts (e.g. the NVIDIA GPU Operator); a no-op concern for
	// OCI charts. Idempotent.
	EnsureRepo(ctx context.Context, name, url string) error
	// Status reads the helm release state. A missing release is not an error.
	Status(ctx context.Context, release, namespace string) (*ChartState, error)
	// UninstallChart removes the helm release. A missing release is not an error.
	UninstallChart(ctx context.Context, release, namespace string) error
	// ApplyKustomize runs `kubectl apply -k dir` against the k3s kubeconfig on
	// the host. Used for overlay CRD instances (kubectl apply -k crds/client/),
	// after the chart so the CRD kinds exist. Idempotent.
	ApplyKustomize(ctx context.Context, dir string) error
	// ApplyManifest applies a Kubernetes manifest (JSON/YAML) via `kubectl apply
	// -f -` over SSH stdin, so secret values never appear in the command string
	// or ps. Used by the secret-sync phase to ensure namespaces + materialize
	// Secrets from operator-local env vars. Idempotent (kubectl apply).
	ApplyManifest(ctx context.Context, manifest string) error
	// DeleteKustomize runs `kubectl delete -k dir` (best-effort; for destroy). A
	// missing resource is not an error.
	DeleteKustomize(ctx context.Context, dir string) error
}
