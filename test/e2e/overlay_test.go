package e2e

import (
	"os"
	"testing"
)

// runOverlayStage upgrades the composed CPU fixture from the migration source
// to the current platform through Forge's production ordering: clone the public
// fixture, install the certificate substrate, establish an exact Flux artifact,
// migrate certificate ownership, apply the platform, then reconcile its CRs.
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

	cfgPath := writeCurrentOverlayForgeConfig(t, state.runID, state.ip, state.privKeyPath, state.chartVersion)
	out := applyWithRetry(t, state.forgeBin, state.forgeHome, cfgPath)
	assertApplyMarkers(t, out, "action:     skip", "node ready: true", "certificate substrate applied: true",
		"chart applied: true", "overlay applied: true", "overlay commit:", "flux installed: true", "gitrepository: ready=True")
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

// writeCurrentOverlayForgeConfig uses the public exact-Flux fixture. Until its
// fixture PR lands on e2e, FORGE_E2E_OVERLAY_REF can select the coordinated
// ticket branch; the committed default returns to e2e before this PR merges.
func writeCurrentOverlayForgeConfig(t *testing.T, name, ip, keyPath, chartVersion string) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, RunLabel: true, DualStack: true,
		ChartVersion: chartVersion,
		OverlayRepo:  "https://github.com/nunocgoncalves/iterabase-overlay.git",
		OverlayRef:   envOr("FORGE_E2E_OVERLAY_REF", "HOR-397-forge-e2e-tool-fixture"),
		Flux:         true,
	})
}
