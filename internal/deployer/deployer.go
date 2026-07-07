// Package deployer defines the cluster-level Helm release operations
// interface — the testability seam for installing/upgrading/uninstalling Helm
// releases (the iterabase-platform chart, the NVIDIA GPU Operator, etc.). The
// real implementation lives in internal/sshprovisioner (helm runs on the host
// over SSH, sharing the same transport as the k3s Provisioner); tests use
// fakes. Lifecycle logic never talks to SSH or helm directly.
package deployer

import "context"

// ChartState is the reconciled state of the platform chart release, read for
// status reporting. A release that does not exist is {Installed: false}.
type ChartState struct {
	Installed bool   // a helm release exists for this name/namespace
	Status    string // helm status: deployed/failed/pending-upgrade/pending-install/...
	Version   string // installed chart version (semver)
}

// Deployer abstracts cluster-level platform chart operations. One instance is
// bound to a single host (the same host the Provisioner bootstrap k3s on); helm
// runs there over SSH using the k3s kubeconfig.
type Deployer interface {
	// Apply idempotently installs or upgrades a Helm release (helm upgrade
	// --install), applying the given --set values (empty for the platform chart,
	// whose values come from the overlay). It ensures helm is present first.
	Apply(ctx context.Context, release, repository, version, namespace string, values []string) error
	// EnsureRepo adds (or force-updates) a Helm repository on the host. Needed
	// for repo-based charts (e.g. the NVIDIA GPU Operator); a no-op concern for
	// OCI charts. Idempotent.
	EnsureRepo(ctx context.Context, name, url string) error
	// Status reads the helm release state. A missing release is not an error.
	Status(ctx context.Context, release, namespace string) (*ChartState, error)
	// UninstallChart removes the helm release. A missing release is not an error.
	UninstallChart(ctx context.Context, release, namespace string) error
}
