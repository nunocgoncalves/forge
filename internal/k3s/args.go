// Package k3s builds k3s install arguments from a forge config.
package k3s

import (
	"sort"

	"github.com/nunocgoncalves/forge/internal/config"
)

// InstallScriptURL is the official k3s install script.
const InstallScriptURL = "https://get.k3s.io"

// ServerArgs builds the k3s `server` flags from a cluster config. Output is
// deterministic (labels sorted by key) so installs are reproducible.
func ServerArgs(cfg *config.Cluster) []string {
	k := cfg.Spec.K3s
	host := cfg.Spec.Hosts[0] // single-node: validated to be exactly one

	args := []string{"server"}
	args = append(args, "--cluster-cidr", DesiredClusterCIDR(k))
	args = append(args, "--service-cidr", DesiredServiceCIDR(k))
	args = append(args, "--flannel-backend=vxlan")

	for _, d := range k.Disable {
		args = append(args, "--disable", d)
	}

	for _, key := range sortedKeys(host.Labels) {
		args = append(args, "--node-label", key+"="+host.Labels[key])
	}
	for _, t := range host.Taints {
		args = append(args, "--node-taint", TaintString(t))
	}

	args = append(args, "--write-kubeconfig-mode", "0600")
	args = append(args, k.ExtraArgs...)
	return args
}

// InstallEnv returns environment variables for the k3s install script.
func InstallEnv(version string) map[string]string {
	return map[string]string{
		"INSTALL_K3S_VERSION": version,
	}
}

// TaintString formats a taint as k3s expects: key=value:effect (or key:effect).
func TaintString(t config.Taint) string {
	if t.Value == "" {
		return t.Key + ":" + t.Effect
	}
	return t.Key + "=" + t.Value + ":" + t.Effect
}

// DesiredClusterCIDR returns the cluster CIDR string k3s stores in config.yaml
// (comma-joined for dual-stack). Used by reconcile to detect immutable drift.
func DesiredClusterCIDR(k config.K3s) string {
	if k.DualStack {
		return k.ClusterCIDR + "," + k.ClusterCIDRv6
	}
	return k.ClusterCIDR
}

// DesiredServiceCIDR returns the service CIDR string k3s stores in config.yaml.
func DesiredServiceCIDR(k config.K3s) string {
	if k.DualStack {
		return k.ServiceCIDR + "," + k.ServiceCIDRv6
	}
	return k.ServiceCIDR
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
