package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// runFluxStage exercises the Flux GitOps phase on the composed CPU fixture:
// droplet: forge installs Flux (flux install), applies a GitRepository +
// Kustomization pointing at the PUBLIC iterabase-overlay base repo (tokenless),
// and Flux source-controller materializes the fork in-cluster + kustomize-controller
// reconciles crds/client. Validates the MECHANICS (install → sync resources →
// Flux reconciles) rather than a writable push-to-git loop (that's Flux upstream
// behavior, validated end-to-end by HOR-299's real OPO1 client fork).
//
// FORGE_OVERLAY_TOKEN is intentionally unset (public repo, CI non-interactive);
// the token→Secret path is covered by unit + fake-SSH tests.
func runFluxStage(t *testing.T, state *digitalOceanCPUState) {
	if _, ok := os.LookupEnv("FORGE_OVERLAY_TOKEN"); ok {
		t.Fatal("FORGE_OVERLAY_TOKEN must be unset for this test (public repo, tokenless)")
	}

	cfgPath := writeFluxForgeConfig(t, state.runID, state.ip, state.privKeyPath, state.chartVersion)
	// The platform chart is already proven by the baseline/overlay stages. Flux
	// owns only continuous overlay reconciliation, so keep this phase isolated.
	out := applyWithRetryArgs(t, state.forgeBin, state.forgeHome, cfgPath, "--skip-chart")
	assertApplyMarkers(t, out, "action:     skip", "node ready: true", "flux installed: true")
	t.Logf("apply output:\n%s", out)

	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
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
	t.Logf("flux gitops reconcile verified on %s", state.ip)
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
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, RunLabel: true, DualStack: true,
		ChartVersion: chartVersion,
		OverlayRepo:  "https://github.com/nunocgoncalves/iterabase-overlay.git",
		OverlayRef:   "master",
		Flux:         true,
	})
}
