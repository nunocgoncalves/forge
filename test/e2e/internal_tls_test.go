package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
)

// runInternalTLS deploys the iterabase-platform umbrella to an isolated Kind
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
//	  -> live transport proofs (not just readiness): the gateway's rendered
//	     DATABASE_URL/REDIS_URL carry sslmode=verify-full + rediss://; a
//	     authenticated plaintext Redis/Postgres attempt is rejected; an unauthenticated
//	     Redis TLS attempt gets NOAUTH; a verified-full Postgres TLS select
//	     succeeds. A regression that renders certs but leaves a link plaintext
//	     fails one of these, not just readiness.
//
// This is the forge e2e home for HOR-371's internal-TLS validation, mirroring
// TestCertIssuers / TestControlPlaneIdentity (the static render check stays in
// iterabase-charts CI as `make check-tls`). The umbrella chart is published at
// oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform; override via
// env for local dev/pinning: ITERABASE_PLATFORM_LOCAL_CHART (helm installs the
// path directly), ITERABASE_CHART_VERSION (pin a release).
func runInternalTLS(t *testing.T) {
	chartRef := envOr("ITERABASE_PLATFORM_CHART", "oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform")
	localChart := os.Getenv("ITERABASE_PLATFORM_LOCAL_CHART") // optional local path for dev
	chartVersion := platformChartVersion(t, localChart)

	namespace := "iterabase-system"
	release := "iterabase"

	// Disable the edge substrate (ingress-nginx + MetalLB) + external-dns + minio
	// — not needed to prove internal TLS, and they'd need a MetalLB pool on kind.
	// Keep control-plane + Postgres + Redis + gateway + cert-manager + cert-issuers.
	edgeOff := map[string]string{
		"ingress-nginx.enabled":            "false",
		"metallb.enabled":                  "false",
		"metallb-config.enabled":           "false",
		"external-dns.enabled":             "false",
		"minio.enabled":                    "false",
		"control-plane.artifact.enabled":   "false",
		"control-plane.toolRunner.enabled": "false",
	}

	// 1. Kind cluster and the platform's version-matched certificate substrate.
	c := kindtest.CreateCluster(t, "forge-internal-tls-e2e")
	installPlatformCertificateSubstrate(t, c, release, chartRef, chartVersion, namespace, localChart)

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

	// Assert the concrete required leaf certs exist (not just that whatever certs
	// exist are Ready): with internal TLS on, postgresql, redis, and the
	// control-plane api must each have an issued Certificate. Matched by name
	// substring so this stays tolerant to the chart's release-prefixed names — a
	// regression that renders certs but drops one component's cert fails loudly
	// here instead of silently passing on an unrelated Ready cert.
	certNames := c.Kubectl(t, "get", "certificates", "-n", namespace,
		"-o", "jsonpath={.items[*].metadata.name}")
	for _, component := range []string{"postgres", "redis", "control-plane"} {
		if !strings.Contains(certNames, component) {
			t.Fatalf("internal TLS on but no Certificate found for %q (certs: %s)", component, certNames)
		}
	}
	t.Logf("required leaf certs present + Ready: %s", certNames)

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

	// 7. Control-plane api serves HTTPS: wait for its TLS rollout before
	//    port-forwarding the Service so a terminating plaintext pod cannot be
	//    selected during the no-wait Helm upgrade. Then GET /healthz with a TLS
	//    client trusting the internal CA. The api cert SAN includes localhost,
	//    so https://localhost:<port> verifies cleanly.
	c.Kubectl(t, "rollout", "status", "-n", namespace,
		"deployment/"+release+"-control-plane-api", "--timeout=300s")
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

	// 8. Gateway is configured for TLS, not just running: inspect its rendered
	//    DATABASE_URL / REDIS_URL env and assert sslmode=verify-full + rediss://.
	//    This is the direct proof of HOR-371 scope items 2 & 3 (connection strings
	//    set sslmode=require-or-stronger; gateway connects with rediss://).
	//    Readiness alone can't distinguish a rediss:// gateway from a plaintext
	//    one; the rendered config can.
	gwPod := c.FirstPodName(t, namespace, "app.kubernetes.io/name=inference-gateway")
	envOut, _ := c.Exec(t, namespace, gwPod, "", "printenv DATABASE_URL REDIS_URL REDIS_TLS_CA_FILE")
	if !strings.Contains(envOut, "sslmode=verify-full") || !strings.Contains(envOut, "sslrootcert=") {
		t.Errorf("gateway DATABASE_URL not configured for verify-full TLS:\n%s", envOut)
	}
	if !strings.Contains(envOut, "rediss://") {
		t.Errorf("gateway REDIS_URL not configured for rediss://:\n%s", envOut)
	}
	t.Logf("gateway rendered TLS config OK (verify-full + rediss://)")

	// 9. Redis live transport: prove the server speaks TLS only and requires
	//    AUTH. Run redis-cli from the redis pod (it ships redis-cli; its own
	//    readiness probe uses it) against the redis Service DNS. With internalTLS
	//    the chart starts redis-server with --tls-port 6379 --port 0 (plaintext
	//    disabled) + --requirepass, so:
	//      - an authenticated plaintext PING is rejected (no RESP on a TLS-only
	//        port; valid creds means rejection is transport, not a missing-auth artifact)
	//      - a TLS PING without auth gets NOAUTH
	//      - a TLS PING with the chart-generated password gets PONG
	//    --insecure skips cert verification (the CA isn't mounted in the server
	//    pod) — it still proves TLS is negotiated, which is the transport claim.
	redisHost := release + "-redis"
	redisPod := c.FirstPodName(t, namespace, "app.kubernetes.io/name=redis")
	redisPW := getSecretKey(t, c, namespace, release+"-redis", "redis-password")

	// Pass the chart password so a NOAUTH response can't mask an accidentally
	// enabled plaintext transport: with TLS-only (--port 0) the server won't
	// negotiate RESP and the PING errors out; only a real plaintext regression
	// returns PONG here.
	plainOut, plainErr := c.Exec(t, namespace, redisPod, "",
		fmt.Sprintf("redis-cli -h %s -p 6379 -a %q PING", redisHost, redisPW))
	if plainErr == nil && strings.Contains(plainOut, "PONG") {
		t.Errorf("redis authenticated plaintext PING succeeded (want TLS-only rejection): %s", plainOut)
	}

	noauthOut, _ := c.Exec(t, namespace, redisPod, "",
		fmt.Sprintf("redis-cli --tls --insecure -h %s -p 6379 PING", redisHost))
	if !strings.Contains(noauthOut, "NOAUTH") {
		t.Errorf("redis TLS PING without auth: want NOAUTH, got: %s", noauthOut)
	}

	authedOut, authedErr := c.Exec(t, namespace, redisPod, "",
		fmt.Sprintf("redis-cli --tls --insecure -h %s -p 6379 -a %q PING", redisHost, redisPW))
	if authedErr != nil || !strings.Contains(authedOut, "PONG") {
		t.Errorf("redis TLS PING with auth: want PONG, got (err=%v): %s", authedErr, authedOut)
	}
	t.Logf("redis live transport OK: plaintext rejected, TLS no-auth -> NOAUTH, TLS+auth -> PONG")

	// 10. Postgres live transport: prove the server requires TLS. The chart runs
	//     postgres with ssl=on + a pg_hba that is `hostssl ... scram-sha-256` /
	//     `host ... reject`, so a sslmode=disable connection is rejected even with
	//     valid credentials, and a sslmode=verify-full connection (trusting the
	//     leaf Secret's ca.crt, which cert-manager populates) succeeds. The
	//     negative probe passes the real password so rejection is transport
	//     (pg_hba hostssl), not a missing-credential artifact. Run psql from the
	//     postgres pod (it ships psql) against the postgres Service DNS, using the
	//     pod's own POSTGRES_USER/DB/PASSWORD env so this is independent of the
	//     exact db.
	pgHost := release + "-postgresql"
	pgPod := c.FirstPodName(t, namespace, "app.kubernetes.io/name=postgresql")
	const pgCA = "/var/lib/postgresql/tls/ca.crt"

	pgPlainOut, pgPlainErr := c.Exec(t, namespace, pgPod, "",
		fmt.Sprintf(`psql "host=%s port=5432 user=$POSTGRES_USER dbname=$POSTGRES_DB password=$POSTGRES_PASSWORD sslmode=disable connect_timeout=5" -c "select 1"`, pgHost))
	if pgPlainErr == nil && !strings.Contains(strings.ToLower(pgPlainOut), "fatal") && !strings.Contains(strings.ToLower(pgPlainOut), "error") {
		t.Errorf("postgres authenticated plaintext select succeeded (want pg_hba rejection): %s", pgPlainOut)
	}

	pgTLSOut, pgTLSErr := c.Exec(t, namespace, pgPod, "",
		fmt.Sprintf(`psql "host=%s port=5432 user=$POSTGRES_USER dbname=$POSTGRES_DB password=$POSTGRES_PASSWORD sslmode=verify-full sslrootcert=%s connect_timeout=5" -c "select 1"`, pgHost, pgCA))
	if pgTLSErr != nil || !strings.Contains(pgTLSOut, "(1 row)") {
		t.Errorf("postgres verify-full select failed (want 1 row, err=%v): %s", pgTLSErr, pgTLSOut)
	}
	t.Logf("postgres live transport OK: plaintext rejected, verify-full select -> 1 row")
}
