package e2e

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
)

// TestCertIssuers deploys the iterabase-platform umbrella with cert-manager +
// the cert-issuers subchart (self-signed ClusterIssuer) to a local Kind cluster,
// simulates the forge secret-sync (applies a dummy cloudflare token Secret in
// the cert-manager controller namespace), and asserts the self-signed
// ClusterIssuer reaches Ready. Proves the HOR-342 chart deploys + cert-manager
// reconciles + the secret-namespace plumbing the HOR-364 secret-sync targets,
// without real Cloudflare credentials (the LE/DNS-01 path is covered by the
// forge unit tests + the DO secret-sync e2e).
//
// The umbrella chart is published at
// oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform with its
// dependencies (cert-manager + cert-issuers) baked in. By default the chart
// version is auto-resolved to the latest stable release; override via env for
// local dev/pinning: ITERABASE_PLATFORM_LOCAL_CHART points at a checkout (helm
// installs the path directly), ITERABASE_CHART_VERSION pins a specific release.
func TestCertIssuers(t *testing.T) {
	chartRef := envOr("ITERABASE_PLATFORM_CHART", "oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform")
	localChart := os.Getenv("ITERABASE_PLATFORM_LOCAL_CHART") // optional local path for dev
	chartVersion := os.Getenv("ITERABASE_CHART_VERSION")
	if chartVersion == "" && localChart == "" {
		chartVersion = kindtest.LatestChartVersion(t, "iterabase-platform")
	}

	namespace := "iterabase-system"

	// 1. Kind cluster.
	c := kindtest.CreateCluster(t, "forge-cert-issuers-e2e")

	// 2. helm install. Install ONLY cert-manager + cert-issuers (disable the
	//    rest) to keep the kind install lean. The umbrella defaults have
	//    cert-manager + cert-issuers (self-signed) enabled; the LE issuer stays
	//    off (no real Cloudflare). The ClusterIssuer is a post-install hook, so
	//    helm --wait (cert-manager Ready) precedes its creation.
	values := map[string]string{
		"inference-gateway.enabled": "false",
		"control-plane.enabled":     "false",
		"redis.enabled":             "false",
		"minio.enabled":             "false",
		"ingress-nginx.enabled":     "false",
		"agent-fleet.enabled":       "false",
		"cert-manager.enabled":      "true",
		"cert-issuers.enabled":      "true",
	}
	c.HelmInstall(t, "iterabase", chartRef, chartVersion, namespace, localChart, values)

	// 3. self-signed ClusterIssuer reaches Ready (cert-manager reconciles the CR
	//    once it is up; the CR lands via the post-install hook after helm --wait).
	c.Kubectl(t, "wait", "--for=condition=Ready", "clusterissuer/selfsigned", "--timeout=180s")

	// 4. Simulate the forge secret-sync: apply the Cloudflare token Secret in
	//    the cert-manager controller namespace (iterabase-system) — the same
	//    namespace the LE issuer's apiTokenSecretRef resolves to (HOR-364
	//    contract). The self-signed issuer does not consume it, but this proves a
	//    Secret can be materialized in the namespace forge's secret-sync targets.
	manifest := `apiVersion: v1
kind: Secret
metadata:
  name: cloudflare-api-token
  namespace: iterabase-system
type: Opaque
stringData:
  api-token: dummy-e2e-value
`
	manifestPath := filepath.Join(t.TempDir(), "secret.yaml")
	mustWriteFile(t, manifestPath, manifest)
	c.Kubectl(t, "apply", "-f", manifestPath)

	// 5. Assert the Secret materialized with the expected value (base64-decoded
	//    .data). This is exactly what forge's secret-sync produces.
	got := c.Kubectl(t, "get", "secret", "cloudflare-api-token", "-n", namespace,
		"-o", "jsonpath={.data.api-token}")
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(got))
	if err != nil {
		t.Fatalf("decode secret data: %v (raw %q)", err, got)
	}
	if string(decoded) != "dummy-e2e-value" {
		t.Fatalf("secret cloudflare-api-token data[api-token] = %q, want %q", decoded, "dummy-e2e-value")
	}
	t.Logf("self-signed ClusterIssuer Ready + cloudflare-api-token Secret materialized in %s", namespace)
}
