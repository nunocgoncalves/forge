package e2e

import (
	"os"
	"testing"
)

// runOverlayStage exercises forge apply --overlay on the composed CPU fixture:
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
func runOverlayStage(t *testing.T, state *digitalOceanCPUState) {
	if _, ok := os.LookupEnv("FORGE_OVERLAY_TOKEN"); ok {
		t.Fatal("FORGE_OVERLAY_TOKEN must be unset for this test (public repo, tokenless)")
	}

	cfgPath := writeOverlayForgeConfig(t, state.runID, state.ip, state.privKeyPath, state.chartVersion)
	out := applyWithRetry(t, state.forgeBin, state.forgeHome, cfgPath)
	assertApplyMarkers(t, out, "action:     skip", "node ready: true", "chart applied: true", "overlay applied: true", "overlay commit:")
	t.Logf("apply output:\n%s", out)

	// The cloned overlay dir exists on the host (a real clone happened).
	overlayDir := "/var/lib/forge/overlay/" + state.runID
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	if _, err := sshOutput(sc, "test -d "+overlayDir+"/.git && test -f "+overlayDir+"/values.yaml"); err != nil {
		t.Fatalf("overlay clone not present on host at %s: %v", overlayDir, err)
	}
}

// writeOverlayForgeConfig writes a forge.yaml identical to the baseline e2e
// config but with the public iterabase-overlay repo configured.
func writeOverlayForgeConfig(t *testing.T, name, ip, keyPath, chartVersion string) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, RunLabel: true, DualStack: true,
		ChartVersion: chartVersion,
		OverlayRepo:  "https://github.com/nunocgoncalves/iterabase-overlay.git",
		OverlayRef:   "e2e",
	})
}
