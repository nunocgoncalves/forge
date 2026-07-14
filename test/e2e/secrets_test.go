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
)

// TestE2ESecrets exercises the forge secret-sync phase end-to-end on an
// ephemeral DO droplet. An overlay (cloned on the host) declares a Secret in
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
func TestE2ESecrets(t *testing.T) {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		t.Skip("DIGITALOCEAN_TOKEN not set; skipping e2e")
	}

	const (
		secretName  = "e2e-test-secret"
		secretNs    = "forge-e2e-secrets"
		secretKey   = "token"
		secretValue = "supersecret-e2e-value"
		envVar      = "FORGE_E2E_TEST_SECRET"
		overlayDir  = "/tmp/forge-secrets-overlay"
	)
	t.Setenv(envVar, secretValue)

	ctx := context.Background()
	client := godo.NewFromToken(token)
	runID := fmt.Sprintf("forge-secrets-e2e-%d", time.Now().Unix())
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

	// Seed a minimal overlay on the host (with a secrets.yaml) + git-init it so
	// forge can clone it via file://. The overlay is the source of truth for the
	// (non-secret) secret declarations (HOR-364).
	sc, err := sshDial(ip, privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", ip, err)
	}
	if err := seedOverlayOnHost(t, sc, overlayDir, secretName, secretNs, secretKey, envVar); err != nil {
		sc.Close()
		t.Fatalf("seed overlay: %v", err)
	}
	sc.Close()

	forgeBin := buildForge(t)
	cfgPath := writeSecretsForgeConfig(t, runID, ip, privKeyPath, overlayDir)
	forgeHome := t.TempDir()

	out := applyWithRetry(t, forgeBin, forgeHome, cfgPath)
	if !strings.Contains(out, "node ready: true") {
		t.Fatalf("apply did not report node ready:\n%s", out)
	}
	if !strings.Contains(out, "secrets applied: true") {
		t.Fatalf("apply did not report secrets applied:\n%s", out)
	}
	t.Logf("apply output:\n%s", out)

	// Verify the Secret was materialized in the cluster with the right value
	// (base64-decoded .data, the same shape forge's secret-sync produces).
	sc2, err := sshDial(ip, privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", ip, err)
	}
	defer sc2.Close()
	got := mustSSHOutput(t, sc2, fmt.Sprintf(
		"sudo k3s kubectl get secret %s -n %s -o jsonpath='{.data.%s}' | base64 -d",
		secretName, secretNs, secretKey))
	if strings.TrimSpace(got) != secretValue {
		t.Fatalf("secret %s/%s data[%s] = %q, want %q", secretNs, secretName, secretKey, got, secretValue)
	}
	gotType := strings.TrimSpace(mustSSHOutput(t, sc2, fmt.Sprintf(
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
  overlay:
    repo: file://%s
    ref: master
  # no chart => chart phase skipped (lean: k3s + overlay secret-sync only)
`, name, ip, keyPath, name, overlayDir)
	p := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
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
