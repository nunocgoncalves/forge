package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forgeConfigSpec is the shared config fixture for cloud E2E stages. Scenario
// writers vary only the fields relevant to the contract they exercise, so the
// common k3s/host recipe cannot drift across files.
type forgeConfigSpec struct {
	Name           string
	Address        string
	SSHKeyPath     string
	RunLabel       bool
	DualStack      bool
	GPU            bool
	ChartVersion   string
	ChartRelease   string
	ChartNamespace string
	OverlayRepo    string
	OverlayRef     string
	Flux           bool
}

func writeForgeConfigSpec(t *testing.T, spec forgeConfigSpec) string {
	t.Helper()
	var cfg strings.Builder
	fmt.Fprintf(&cfg, `apiVersion: forge.horizonshift.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  mode: single-node
  hosts:
    - address: %s
      sshUser: forge
      sshKeyPath: %s
      role: control-plane+worker
`, spec.Name, spec.Address, spec.SSHKeyPath)
	if spec.RunLabel {
		fmt.Fprintf(&cfg, `      labels:
        e2e.horizonshift.io/run: %q
`, spec.Name)
	}
	fmt.Fprintf(&cfg, `  k3s:
    version: v1.31.5+k3s1
    clusterCIDR: 10.42.0.0/16
    serviceCIDR: 10.43.0.0/16
    dualStack: %t
`, spec.DualStack)
	if spec.DualStack {
		cfg.WriteString(`    clusterCIDRv6: fd42::/48
    serviceCIDRv6: fd43::/112
    disable: [traefik, servicelb]
`)
	}
	if spec.GPU {
		cfg.WriteString(`  gpu:
    enabled: true
`)
	}
	if spec.ChartVersion != "" {
		fmt.Fprintf(&cfg, `  chart:
    version: %s
`, spec.ChartVersion)
		if spec.ChartRelease != "" {
			fmt.Fprintf(&cfg, "    release: %s\n", spec.ChartRelease)
		}
		if spec.ChartNamespace != "" {
			fmt.Fprintf(&cfg, "    namespace: %s\n", spec.ChartNamespace)
		}
	}
	if spec.OverlayRepo != "" {
		fmt.Fprintf(&cfg, `  overlay:
    repo: %s
    ref: %s
`, spec.OverlayRepo, spec.OverlayRef)
	}
	if spec.Flux {
		cfg.WriteString(`  flux:
    enabled: true
    version: "v2.4.0"
`)
	}

	path := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(path, []byte(cfg.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
