package e2e

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// runSecretsStage exercises forge secret-sync on the composed CPU fixture. An
// overlay cloned on the host declares a Secret in
// secrets.yaml; forge reads it, resolves the value from an operator env var, and
// materializes the Secret via `kubectl apply -f -` over SSH stdin — the value
// never appears in the command line. Lean run (no chart) to isolate the
// secret-sync mechanics. Validates the HOR-364 path cert-manager (cert-issuers,
// HOR-342) + external-dns (HOR-343) will rely on.
//
// The overlay is seeded directly on the host (a minimal scaffold + secrets.yaml,
// git-init'd) and referenced via file:// so this test is self-contained (no
// external overlay repo required). A real install points overlay.repo at the
// client-fork overlay git URL instead.
func runSecretsStage(t *testing.T, state *digitalOceanCPUState) {
	const (
		secretName  = "e2e-test-secret"
		secretNs    = "forge-e2e-secrets"
		secretKey   = "token"
		secretValue = "supersecret-e2e-value"
		envVar      = "FORGE_E2E_TEST_SECRET"
		overlayDir  = "/tmp/forge-secrets-overlay"
	)
	t.Setenv(envVar, secretValue)

	// Seed a minimal overlay on the existing forge host. Secret-sync only needs
	// a ready k3s substrate; provisioning another VM would not add a boundary.
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	if err := seedOverlayOnHost(t, sc, overlayDir, secretName, secretNs, secretKey, envVar); err != nil {
		sc.Close()
		t.Fatalf("seed overlay: %v", err)
	}
	sc.Close()

	cfgPath := writeSecretsForgeConfig(t, state.runID, state.ip, state.privKeyPath, overlayDir)
	out := applyWithRetry(t, state.forgeBin, state.forgeHome, cfgPath)
	assertApplyMarkers(t, out, "action:     skip", "node ready: true", "secrets applied: true")
	t.Logf("apply output:\n%s", out)

	sc, err = sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	got := mustSSHOutput(t, sc, fmt.Sprintf(
		"sudo k3s kubectl get secret %s -n %s -o jsonpath='{.data.%s}' | base64 -d",
		secretName, secretNs, secretKey))
	if strings.TrimSpace(got) != secretValue {
		t.Fatalf("secret %s/%s data[%s] = %q, want %q", secretNs, secretName, secretKey, got, secretValue)
	}
	gotType := strings.TrimSpace(mustSSHOutput(t, sc, fmt.Sprintf(
		"sudo k3s kubectl get secret %s -n %s -o jsonpath='{.type}'", secretName, secretNs)))
	if gotType != "Opaque" {
		t.Fatalf("secret type = %q, want Opaque", gotType)
	}
	t.Logf("secret %s/%s materialized with the expected value + type", secretNs, secretName)
}

// seedOverlayOnHost creates a minimal overlay (values.yaml, values.client.yaml,
// crds/client/kustomization.yaml, secrets.yaml) at dir on the host, ensures git
// is present, and git-init/commits it so forge can `git clone file://<dir>`.
func seedOverlayOnHost(t *testing.T, sc *ssh.Client, dir, name, ns, key, envVar string) error {
	t.Helper()
	if _, err := sshOutput(sc, "if ! command -v git >/dev/null 2>&1; then sudo apt-get update -qq && sudo apt-get install -y git; fi"); err != nil {
		return fmt.Errorf("ensure git: %w", err)
	}
	manifest := fmt.Sprintf(`set -e
rm -rf %[1]s
mkdir -p %[1]s/crds/client
cat > %[1]s/values.yaml <<'EOF'
# base values (scaffold)
EOF
cat > %[1]s/values.client.yaml <<'EOF'
# client values (scaffold)
EOF
cat > %[1]s/crds/client/kustomization.yaml <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
EOF
cat > %[1]s/secrets.yaml <<'EOF'
secrets:
  - name: %[2]s
    namespace: %[3]s
    key: %[4]s
    envVar: %[5]s
EOF
git init -q -b master %[1]s
git -C %[1]s add -A
git -C %[1]s -c user.email=e2e@forge -c user.name=e2e commit -q -m init
`, dir, name, ns, key, envVar)
	if _, err := sshOutput(sc, manifest); err != nil {
		return fmt.Errorf("write overlay: %w", err)
	}
	return nil
}

// writeSecretsForgeConfig writes a lean forge.yaml (k3s + overlay secret-sync,
// no chart) pointing overlay.repo at the host-local seeded overlay dir.
func writeSecretsForgeConfig(t *testing.T, name, ip, keyPath, overlayDir string) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, RunLabel: true, DualStack: true,
		OverlayRepo: "file://" + overlayDir, OverlayRef: "master",
	})
}

// mustSSHOutput runs a command over SSH and fails the test on error.
func mustSSHOutput(t *testing.T, sc *ssh.Client, cmd string) string {
	t.Helper()
	out, err := sshOutput(sc, cmd)
	if err != nil {
		t.Fatalf("ssh %q: %v\n%s", cmd, err, out)
	}
	return out
}
