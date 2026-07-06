// Package provisioner defines the host-level k3s operations interface.
//
// Provisioner is the testability seam: the lifecycle orchestrates against this
// interface, the real implementation lives in internal/sshprovisioner, and
// tests use fakes. Lifecycle logic must never talk to SSH directly.
package provisioner

import (
	"context"

	"github.com/nunocgoncalves/forge/internal/config"
)

// HostState is the actual state of k3s on a host, read for reconcile.
type HostState struct {
	Installed   bool              // k3s is installed on the host
	Version     string            // k3s version, e.g. "v1.31.5+k3s1"
	ClusterCIDR string            // as stored in config.yaml (comma-joined for dual-stack)
	ServiceCIDR string
	DualStack   bool
	Labels      map[string]string // current node labels
	Taints      []config.Taint    // current node taints
}

// PreflightResult is the read-only host readiness check outcome.
type PreflightResult struct {
	OS         string // e.g. "Ubuntu 24.04"
	HasSudo    bool   // passwordless sudo works
	HasCurl    bool   // curl present (install script dependency)
	HasSystemd bool   // systemd present (k3s is a systemd unit)
	Installed  bool   // k3s already installed
	HasIPv6    bool   // host has IPv6 (relevant when dualStack)
}

// Provisioner abstracts host-level k3s operations. One instance is bound to a
// single host at construction time (the SSH user/key/address).
type Provisioner interface {
	// Preflight runs read-only readiness checks against the host.
	Preflight(ctx context.Context) (*PreflightResult, error)
	// Install installs k3s with the given server args and version.
	Install(ctx context.Context, version string, serverArgs []string) error
	// Upgrade upgrades k3s in-place to the given version via the install script.
	Upgrade(ctx context.Context, version string, serverArgs []string) error
	// Uninstall runs k3s-uninstall.sh on the host.
	Uninstall(ctx context.Context) error
	// FetchKubeconfig reads /etc/rancher/k3s/k3s.yaml from the host.
	FetchKubeconfig(ctx context.Context) ([]byte, error)
	// ReadState reads the actual k3s state for reconcile.
	ReadState(ctx context.Context) (*HostState, error)
}
