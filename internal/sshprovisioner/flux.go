package sshprovisioner

import (
	"context"
	"fmt"
	"strings"

	"github.com/nunocgoncalves/forge/internal/fluxer"
)

// Compile-time assertion: SSHProvisioner implements fluxer.Fluxer.
var _ fluxer.Fluxer = (*SSHProvisioner)(nil)

const fluxInstallScript = "https://fluxcd.io/install.sh"

// EnsureFlux implements fluxer.Fluxer. Installs the flux CLI on the host if
// absent (the official install script, version-pinned via FLUX_VERSION), then
// runs `flux install` to apply the Flux components + CRDs into the cluster
// against the k3s kubeconfig. Idempotent.
func (p *SSHProvisioner) EnsureFlux(ctx context.Context, version string) error {
	if _, err := p.run(ctx, "command -v flux"); err != nil {
		// The official install script prepends "v" to FLUX_VERSION internally when
		// building the release URL, so pass the version WITHOUT the leading "v"
		// (the config stores the release tag "vX.Y.Z"). flux install --version
		// below takes the tag verbatim (with "v").
		fluxVer := strings.TrimPrefix(version, "v")
		if _, err := p.run(ctx, fmt.Sprintf("curl -fsSL %s | sudo FLUX_VERSION=%s bash", fluxInstallScript, shellQuote(fluxVer))); err != nil {
			return fmt.Errorf("install flux cli: %w", err)
		}
	}
	if _, err := p.run(ctx, fluxCmd("install", "--version="+version)); err != nil {
		return fmt.Errorf("flux install: %w", err)
	}
	return nil
}

// UninstallFlux implements fluxer.Fluxer. Best-effort `flux uninstall`
// (non-interactive, --silent skips the confirmation prompt + suppresses info
// output) — removes Flux components + CRDs + flux-system resources. A missing
// Flux install (or absent flux CLI) is not an error so destroy always proceeds
// to k3s removal (which wipes the cluster regardless).
func (p *SSHProvisioner) UninstallFlux(ctx context.Context) error {
	if _, err := p.run(ctx, "command -v flux"); err != nil {
		return nil // flux CLI absent => nothing to remove
	}
	_, _ = p.run(ctx, fluxCmd("uninstall", "--silent")) // best-effort
	return nil
}

// GitRepositoryStatus implements fluxer.Fluxer. It reports the Ready condition
// plus Flux's exact materialized revision and content digest (HOR-397). This is
// informational and never gates apply; Forge neither downloads nor parses tool
// code. Missing/not-yet-ready status remains a tolerated empty result.
func (p *SSHProvisioner) GitRepositoryStatus(ctx context.Context, name string) (string, error) {
	out, err := p.run(ctx, kubectlCmd("get", "gitrepository", "-n", "flux-system", name,
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}{"\t"}{.status.artifact.revision}{"\t"}{.status.artifact.digest}`))
	if err != nil {
		return "", nil // CR not present yet or transient error — tolerate
	}
	parts := strings.Split(strings.TrimSpace(out), "\t")
	if len(parts) == 0 || parts[0] == "" {
		return "", nil
	}
	status := "ready=" + parts[0]
	if len(parts) > 1 && parts[1] != "" {
		status += " revision=" + parts[1]
	}
	if len(parts) > 2 && parts[2] != "" {
		status += " digest=" + parts[2]
	}
	return status, nil
}

// fluxCmd builds a sudo flux command targeting the k3s kubeconfig on the host.
// The flux CLI has no --kubeconfig flag; it reads KUBECONFIG, so it is exported
// via env. sudo runs as root (the k3s kubeconfig at /etc/rancher/k3s/k3s.yaml is
// root-owned 0600), and the in-process server URL (127.0.0.1:6443) is reachable
// on the host — no rewrite needed (unlike off-host kubeconfig use).
func fluxCmd(args ...string) string {
	return joinArgs(append([]string{"sudo", "env", "KUBECONFIG=" + k3sKubeconfigPath, "flux"}, args...))
}
