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
	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
)

// TestE2EOverlay exercises forge apply --overlay end-to-end on an ephemeral DO
// droplet: it clones the PUBLIC iterabase-overlay base repo tokenlessly on the
// host, feeds its value files to the platform chart (helm -f), and applies its
// CRD instances (kubectl apply -k crds/client/). The scaffold's crds/client/ is
// empty + values are comment-only, so this validates the MECHANICS on a real
// host (clone → helm -f → kubectl apply -k) rather than a specific instance.
//
// It points at ref `e2e` (a minimal-scaffold test-fixture branch): `master` holds
// the HOR-299 bare-metal prod recipe (required placeholders, not deployable bare
// on a cloud VM). The prod recipe's deployability is HOR-299's job; this test is
// forge's mechanics. See iterabase-overlay `e2e` branch.
//
// FORGE_OVERLAY_TOKEN is intentionally unset (public repo, CI non-interactive);
// the token-prompt path is covered by unit + fake-SSH tests.
func TestE2EOverlay(t *testing.T) {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		t.Skip("DIGITALOCEAN_TOKEN not set; skipping e2e")
	}
	if _, ok := os.LookupEnv("FORGE_OVERLAY_TOKEN"); ok {
		t.Fatal("FORGE_OVERLAY_TOKEN must be unset for this test (public repo, tokenless)")
	}

	ctx := context.Background()
	client := godo.NewFromToken(token)
	runID := fmt.Sprintf("forge-overlay-e2e-%d", time.Now().Unix())
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

	cfgPath := writeOverlayForgeConfig(t, runID, ip, privKeyPath, chartVersion)
	forgeHome := t.TempDir()

	// FORGE_OVERLAY_TOKEN unset => forge proceeds tokenless (public repo, CI).
	out := applyWithRetry(t, forgeBin, forgeHome, cfgPath)
	if !strings.Contains(out, "node ready: true") {
		t.Fatalf("apply did not report node ready:\n%s", out)
	}
	if !strings.Contains(out, "chart applied: true") {
		t.Fatalf("apply did not report chart applied:\n%s", out)
	}
	if !strings.Contains(out, "overlay applied: true") {
		t.Fatalf("apply did not report overlay applied:\n%s", out)
	}
	if !strings.Contains(out, "overlay commit:") {
		t.Fatalf("apply did not report overlay commit:\n%s", out)
	}
	t.Logf("apply output:\n%s", out)

	// The cloned overlay dir exists on the host (a real clone happened).
	overlayDir := "/var/lib/forge/overlay/" + runID
	sc, err := sshDial(ip, privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", ip, err)
	}
	defer sc.Close()
	if _, err := sshOutput(sc, "test -d "+overlayDir+"/.git && test -f "+overlayDir+"/values.yaml"); err != nil {
		t.Fatalf("overlay clone not present on host at %s: %v", overlayDir, err)
	}
}

// writeOverlayForgeConfig writes a forge.yaml identical to the baseline e2e
// config but with the public iterabase-overlay repo configured.
func writeOverlayForgeConfig(t *testing.T, name, ip, keyPath, chartVersion string) string {
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
    ref: e2e
`, name, ip, keyPath, name, chartVersion)
	p := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}
