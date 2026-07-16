package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
)

// TestE2EFlux exercises the Flux GitOps phase end-to-end on an ephemeral DO
// droplet: forge installs Flux (flux install), applies a GitRepository +
// Kustomization pointing at the PUBLIC iterabase-overlay base repo (tokenless),
// and Flux source-controller materializes the fork in-cluster + kustomize-controller
// reconciles crds/client. Validates the MECHANICS (install → sync resources →
// Flux reconciles) rather than a writable push-to-git loop (that's Flux upstream
// behavior, validated end-to-end by HOR-299's real OPO1 client fork).
//
// FORGE_OVERLAY_TOKEN is intentionally unset (public repo, CI non-interactive);
// the token→Secret path is covered by unit + fake-SSH tests.
func TestE2EFlux(t *testing.T) {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		t.Skip("DIGITALOCEAN_TOKEN not set; skipping e2e")
	}
	if _, ok := os.LookupEnv("FORGE_OVERLAY_TOKEN"); ok {
		t.Fatal("FORGE_OVERLAY_TOKEN must be unset for this test (public repo, tokenless)")
	}

	ctx := context.Background()
	client := godo.NewFromToken(token)
	runID := fmt.Sprintf("forge-flux-e2e-%d", time.Now().Unix())
	keep := os.Getenv("FORGE_E2E_KEEP") != ""
	t.Logf("run %s (keep=%v)", runID, keep)

	pubKeyStr, privKeyPath := generateKey(t)

	d, err := createDroplet(ctx, client, runID, pubKeyStr)
	if err != nil {
		t.Fatalf("create droplet: %v", err)
	}
	defer func() {
		if keep {
			t.Logf("keeping droplet %d for debugging", d.ID)
			return
		}
		deleteDroplet(ctx, client, d.ID)
	}()
	ip, err := waitForIP(ctx, client, d.ID)
	if err != nil {
		t.Fatalf("wait for IP: %v", err)
	}
	t.Logf("droplet ip %s", ip)
	if err := waitForHostReady(ctx, ip, privKeyPath); err != nil {
		t.Fatalf("host never became ready: %v", err)
	}

	forgeBin := buildForge(t)

	chartVersion := os.Getenv("ITERABASE_CHART_VERSION")
	if chartVersion == "" {
		chartVersion = kindtest.LatestChartVersion(t, "iterabase-platform")
	}

	cfgPath := writeFluxForgeConfig(t, runID, ip, privKeyPath, chartVersion)
	forgeHome := t.TempDir()

	// FORGE_OVERLAY_TOKEN unset => forge proceeds tokenless (public repo); the
	// GitRepository has no secretRef (Flux clones anonymously).
	out := applyWithRetry(t, forgeBin, forgeHome, cfgPath)
	if !strings.Contains(out, "node ready: true") {
		t.Fatalf("apply did not report node ready:\n%s", out)
	}
	if !strings.Contains(out, "flux installed: true") {
		t.Fatalf("apply did not report flux installed:\n%s", out)
	}
	t.Logf("apply output:\n%s", out)

	sc, err := sshDial(ip, privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", ip, err)
	}
	defer sc.Close()

	// Flux source-controller + kustomize-controller pods are Running.
	pods, err := sshOutput(sc, "sudo k3s kubectl get pods -n flux-system --no-headers")
	if err != nil {
		t.Fatalf("get flux-system pods: %v\n%s", err, pods)
	}
	if !strings.Contains(pods, "source-controller") || !strings.Contains(pods, "kustomize-controller") {
		t.Fatalf("flux-system missing source/kustomize controller pods:\n%s", pods)
	}
	for _, line := range strings.Split(strings.TrimSpace(pods), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "Running") && !strings.Contains(line, "Completed") {
			t.Fatalf("flux-system pod not Running:\n%s", line)
		}
	}

	// GitRepository becomes Ready + source-controller materializes the fork
	// (.status.artifact.revision non-empty) — the HOR-351 in-cluster source
	// contract. Poll: Flux reconciles async after the GitRepository is applied.
	gitReady, gitArtifact := pollFluxReady(t, sc, "gitrepository", "overlay", 4*time.Minute)
	if !gitReady {
		t.Fatalf("GitRepository overlay never reached Ready=True")
	}
	t.Logf("gitrepository artifact revision: %s", gitArtifact)
	if gitArtifact == "" {
		t.Fatalf("GitRepository overlay has no materialized artifact (.status.artifact.revision empty) — source-controller did not fetch the fork")
	}

	// Kustomization becomes Ready (reconciled crds/client — empty scaffold, 0
	// objects, but Healthy wiring end-to-end).
	kustReady, _ := pollFluxReady(t, sc, "kustomization", "overlay-crds", 2*time.Minute)
	if !kustReady {
		t.Fatalf("Kustomization overlay-crds never reached Ready=True")
	}
	t.Logf("flux gitops reconcile verified on %s", ip)
}

// pollFluxReady polls a Flux CR's Ready condition until True or the timeout
// elapses. For a GitRepository it also reads .status.artifact.revision (the
// source-controller materialization proof) and returns it.
func pollFluxReady(t *testing.T, client *ssh.Client, kind, name string, timeout time.Duration) (ready bool, artifact string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	condPath := `'{.status.conditions[?(@.type=="Ready")].status}'`
	for {
		out, err := sshOutput(client, fmt.Sprintf("sudo k3s kubectl get %s -n flux-system %s -o jsonpath=%s", kind, name, condPath))
		if err == nil && strings.TrimSpace(out) == "True" {
			ready = true
			if kind == "gitrepository" {
				art, _ := sshOutput(client, fmt.Sprintf("sudo k3s kubectl get %s -n flux-system %s -o jsonpath='{.status.artifact.revision}'", kind, name))
				artifact = strings.TrimSpace(art)
			}
			return ready, artifact
		}
		if time.Now().After(deadline) {
			t.Logf("timeout waiting for %s/%s Ready (last output: %q)", kind, name, strings.TrimSpace(out))
			return false, ""
		}
		time.Sleep(10 * time.Second)
	}
}

// writeFluxForgeConfig writes a forge.yaml identical to the overlay e2e config
// but with Flux enabled (pointing at the public iterabase-overlay base repo).
func writeFluxForgeConfig(t *testing.T, name, ip, keyPath, chartVersion string) string {
	t.Helper()
	cfg := fmt.Sprintf(`apiVersion: forge.horizonshift.io/v1alpha1
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
      labels:
        e2e.horizonshift.io/run: "%s"
  k3s:
    version: v1.31.5+k3s1
    clusterCIDR: 10.42.0.0/16
    serviceCIDR: 10.43.0.0/16
    dualStack: true
    clusterCIDRv6: fd42::/48
    serviceCIDRv6: fd43::/112
    disable: [traefik, servicelb]
  chart:
    version: %s
  overlay:
    repo: https://github.com/nunocgoncalves/iterabase-overlay.git
    ref: master
  flux:
    enabled: true
    version: "v2.4.0"
`, name, ip, keyPath, name, chartVersion)
	p := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}
