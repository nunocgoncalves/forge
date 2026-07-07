// Package provisioner defines the host-level k3s operations interface.
//
// Provisioner is the testability seam: the lifecycle orchestrates against this
// interface, the real implementation lives in internal/sshprovisioner, and
// tests use fakes. Lifecycle logic must never talk to SSH directly.
package provisioner

import "context"

// HostState is the actual host-level state of k3s, read for reconcile.
// Node-level state (labels/taints) is applied at install time via k3s flags in
// v1 and reconciled via the API in a later version.
type HostState struct {
	Installed   bool   // k3s is installed on the host
	Version     string // k3s version, e.g. "v1.31.5+k3s1"
	ClusterCIDR string // as stored in config.yaml (comma-joined for dual-stack)
	ServiceCIDR string
	DualStack   bool
}

// PreflightResult is the read-only host readiness check outcome.
type PreflightResult struct {
	OS                     string // e.g. "Ubuntu 24.04 LTS"
	HasSudo                bool   // passwordless sudo works
	HasCurl                bool   // curl present (install script dependency)
	HasSystemd             bool   // systemd present (k3s is a systemd unit)
	Installed              bool   // k3s already installed
	HasIPv6                bool   // host has IPv6 (relevant when dualStack)
	HasNVIDIAGPU           bool   // an NVIDIA GPU is on the PCI bus (GPU preflight; S11 passthrough precondition)
	KernelHeadersInstalled bool   // linux-headers-$(uname -r) present (GPU driver build dep)
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
	// ReadState reads the actual host-level k3s state for reconcile.
	ReadState(ctx context.Context) (*HostState, error)
	// NodeReady reports whether the cluster node is Ready (via remote k3s kubectl).
	NodeReady(ctx context.Context) (bool, error)
	// EnsureDriverBuildDeps ensures the host can compile the NVIDIA kernel module
	// via the GPU operator's driver container (installs matching linux-headers on
	// Ubuntu). Idempotent. Only called when GPU is enabled.
	EnsureDriverBuildDeps(ctx context.Context) error
	// GPUReady reports whether the GPU operator's ClusterPolicy has reached
	// state=ready (driver + toolkit + device plugin + runtime + CUDA validated
	// end-to-end by the operator). Polled as the GPU readiness gate.
	GPUReady(ctx context.Context) (bool, error)
}
