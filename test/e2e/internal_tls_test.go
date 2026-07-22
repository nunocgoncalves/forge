package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
)

// TestInternalTLS deploys the iterabase-platform umbrella to a local Kind
// cluster with the single internalTLS switch on (global.internalTLS.enabled),
// and proves the in-cluster TLS plane works end-to-end off the private CA:
//
//	phase 1: helm install plaintext (--wait) — brings cert-manager Ready
//	phase 2: helm upgrade with internal TLS on (no --wait; the cert hooks
//	         would deadlock under --wait — pods wait on the hook-issued
//	         Secrets, but --wait waits for pods before running hooks)
//	  -> internal CA ClusterIssuer Ready
//	  -> component leaf certs (postgresql/redis/control-plane-api) Ready
//	  -> gateway pod Ready (startup rdb.Ping proves Redis TLS: rediss:// + CA)
//	  -> gateway /readyz 200 over port-forward (snapshot fresh -> Postgres verify-full)
//	  -> control-plane api /healthz 200 over HTTPS (TLS client trusting the CA;
//	     the api cert SAN includes localhost for the port-forward)
//
// This is the forge e2e home for HOR-371's internal-TLS validation, mirroring
// TestCertIssuers / TestControlPlaneIdentity (the static render check stays in
// iterabase-charts CI as `make check-tls`). The umbrella chart is published at
// oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform; override via
// env for local dev/pinning: ITERABASE_PLATFORM_LOCAL_CHART (helm installs the
// path directly), ITERABASE_CHART_VERSION (pin a release).
func TestInternalTLS(t *testing.T) {
	chartRef := envOr("ITERABASE_PLATFORM_CHART", "oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform")
	localChart := os.Getenv("ITERABASE_PLATFORM_LOCAL_CHART") // optional local path for dev
	chartVersion := os.Getenv("ITERABASE_CHART_VERSION")
	if chartVersion == "" && localChart == "" {
		chartVersion = kindtest.LatestChartVersion(t, "iterabase-platform")
	}

	namespace := "iterabase-system"
	release := "iterabase"

	// Disable the edge substrate (ingress-nginx + MetalLB) + external-dns + minio
	// — not needed to prove internal TLS, and they'd need a MetalLB pool on kind.
	// Keep control-plane + Postgres + Redis + gateway + cert-manager + cert-issuers.
	edgeOff := map[string]string{
		"ingress-nginx.enabled":  "false",
		"metallb.enabled":        "false",
		"metallb-config.enabled": "false",
		"external-dns.enabled":   "false",
		"minio.enabled":          "false",
	}

	// 1. Kind cluster.
	c := kindtest.CreateCluster(t, "forge-internal-tls-e2e")

	// 2. Phase 1: install plaintext (--wait brings cert-manager Ready + all
	//    components up over plain TCP — no certs needed yet).
	c.HelmInstall(t, release, chartRef, chartVersion, namespace, localChart, edgeOff)

	// 3. Phase 2: upgrade with internal TLS on (no --wait — the cert hooks run
	//    now that cert-manager is Ready, and the pods roll to mount them).
	tlsOn := map[string]string{"global.internalTLS.enabled": "true"}
	for k, v := range edgeOff {
		tlsOn[k] = v
	}
	c.HelmUpgrade(t, release, chartRef, chartVersion, namespace, localChart, tlsOn)

	// 4. Internal CA ClusterIssuer Ready (cert-manager issued the root CA via the
	//    post-upgrade hook), then all component leaf certs Ready.
	c.Kubectl(t, "wait", "--for=condition=Ready", "clusterissuer/internal-ca", "--timeout=180s")
	c.Kubectl(t, "wait", "--for=jsonpath={.status.conditions[?(@.type==\"Ready\")].status}=True",
		"certificate", "-n", namespace, "--all", "--timeout=180s")

	// 5. Gateway pod Ready. Its startup rdb.Ping proves Redis TLS (rediss:// +
	//    the mounted CA); Available implies the snapshot (Postgres verify-full)
	//    is fresh. kubectl fatals (with the rollout message) on timeout.
	c.Kubectl(t, "rollout", "status", "-n", namespace,
		"deployment/"+release+"-gateway", "--timeout=300s")

	// 6. Gateway /readyz 200 over port-forward (HTTP — the gateway isn't an
	//    internal TLS server; this proves Postgres verify-full via the snapshot).
	gwBase, _ := c.PortForward(t, namespace, "svc/"+release+"-gateway", 8080, 18080)
	gwBody := mustGet(t, kindtest.HTTPClient(), gwBase+"/readyz", "")
	if !strings.Contains(gwBody, `"fresh":true`) {
		t.Fatalf("gateway /readyz snapshot not fresh: %s", gwBody)
	}
	t.Logf("gateway /readyz 200 — Postgres verify-full + Redis TLS green")

	// 7. Control-plane api serves HTTPS: port-forward the api Service and GET
	//    /healthz with a TLS client trusting the internal CA. The api cert SAN
	//    includes localhost, so https://localhost:<port> verifies cleanly.
	caB64 := c.Kubectl(t, "get", "secret", release+"-internal-ca-root", "-n", namespace,
		"-o", "jsonpath={.data.ca\\.crt}")
	caPEM, err := base64.StdEncoding.DecodeString(strings.TrimSpace(caB64))
	if err != nil {
		t.Fatalf("decode internal CA ca.crt: %v (raw %q)", err, caB64)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("internal CA ca.crt parsed no certs: %s", caPEM)
	}
	apiClient := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}, //nolint:gosec // test CA
	}
	c.PortForward(t, namespace, "svc/"+release+"-control-plane-api", 8080, 18081)
	resp, err := apiClient.Get("https://localhost:18081/healthz")
	if err != nil {
		t.Fatalf("control-plane api HTTPS /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control-plane api HTTPS /healthz: status %d (want 200)", resp.StatusCode)
	}
	t.Logf("control-plane api HTTPS /healthz 200 — verified against the internal CA")
}
