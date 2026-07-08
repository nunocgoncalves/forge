package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
)

// TestControlPlaneIdentity deploys the control-plane Helm chart to a local Kind
// cluster and exercises the full identity flow end-to-end:
//
//	deploy chart (pgvector Postgres + manager + api + migrate + bootstrap)
//	  -> capture admin + agent-fleet keys from bootstrap init-container logs
//	  -> apply IdentityMapping CR (linked Teams user) -> wait status.ready=true
//	  -> GET /.well-known/jwks.json -> 200 + keys
//	  -> POST /v1/token (agent-fleet key + linked surface user) -> 200 + JWT
//	  -> verify the JWT signature against JWKS
//	  -> delete IdentityMapping -> POST /v1/token for that user -> 403 (soft-delete)
//
// This establishes the forge e2e pattern for future control-plane CRDs
// (PermissionPolicy / Model / ModelBackend / AgentSandbox): the reusable
// harness lives in test/e2e/internal/kindtest.
//
// The control-plane chart (HOR-316) is published at
// oci://ghcr.io/nunocgoncalves/iterabase-charts/control-plane with its pgvector
// Postgres dependency baked in. By default the chart version is auto-resolved
// from the charts repo's latest stable GitHub release and the image tag is
// derived from the chart's appVersion, so CI is never silently pinned to an old
// control-plane and the image can't drift from the chart (HOR-321). Override via
// env for local dev / pinning: CONTROL_PLANE_LOCAL_CHART points at a checkout
// (helm installs the path directly), CONTROL_PLANE_LOCAL_IMAGE loads a
// locally-built image into the Kind nodes, and CONTROL_PLANE_CHART_VERSION /
// CONTROL_PLANE_IMAGE_TAG pin a specific release/tag.
func TestControlPlaneIdentity(t *testing.T) {
	// --- chart config (env-overridable) ---
	chartRef := envOr("CONTROL_PLANE_CHART", "oci://ghcr.io/nunocgoncalves/iterabase-charts/control-plane")
	localChart := os.Getenv("CONTROL_PLANE_LOCAL_CHART") // optional local path for dev
	imageRepo := envOr("CONTROL_PLANE_IMAGE_REPO", "ghcr.io/nunocgoncalves/control-plane")

	// chartVersion: explicit pin wins; otherwise auto-resolve the latest stable
	// release from the charts repo's GitHub releases (HOR-321). Skipped for
	// local-chart dev — helm installs the path directly, so no version is needed.
	chartVersion := os.Getenv("CONTROL_PLANE_CHART_VERSION")
	if chartVersion == "" && localChart == "" {
		chartVersion = kindtest.LatestChartVersion(t, "control-plane")
	}

	// imageTag: explicit pin wins; otherwise derive it from the chart's
	// appVersion so the deployed image can never drift from the chart (the
	// control-plane chart keeps appVersion == service version == image tag, per
	// HOR-317). Reads the local Chart.yaml when CONTROL_PLANE_LOCAL_CHART is set.
	imageTag := os.Getenv("CONTROL_PLANE_IMAGE_TAG")
	if imageTag == "" {
		imageTag = kindtest.ChartAppVersion(t, chartRef, chartVersion, localChart)
	}

	namespace := "control-plane-system"
	release := "cp"

	// Chart-coupled names (confirmed against the shipped control-plane chart,
	// HOR-316). These are the only chart-specific bits; everything else is flow.
	const (
		apiSelector   = "app.kubernetes.io/component=api" // api pod label selector
		bootstrapCont = "bootstrap"                       // init container that prints keys
		apiService    = "svc/cp-control-plane-api"        // api Service (kubectl target)
		apiPort       = 8080
	)

	// 1. Kind cluster.
	c := kindtest.CreateCluster(t, "forge-cp-e2e")

	// (optional) load a locally-built control-plane image for dev.
	if localImage := os.Getenv("CONTROL_PLANE_LOCAL_IMAGE"); localImage != "" {
		c.LoadImage(t, localImage)
	}

	// 2. helm install. Values configure a self-contained, pgvector-backed,
	//    enrolled-mode install. Value keys match the shipped chart's values.yaml.
	values := map[string]string{
		"image.repository":             imageRepo,
		"image.tag":                    imageTag,
		"image.pullPolicy":             "IfNotPresent",
		"identity.mode":                "enrolled",
		"postgresql.enabled":           "true",
		"postgresql.image.repository":  "pgvector/pgvector",
		"postgresql.image.tag":         "pg16",
		"bootstrap.serviceAccounts[0]": "agent-fleet",
	}
	c.HelmInstall(t, release, chartRef, chartVersion, namespace, localChart, values)

	// 3. capture bootstrap keys from the api pod's bootstrap init container.
	//    helm --wait guarantees init containers completed before Ready.
	apiPod := c.FirstPodName(t, namespace, apiSelector)
	logs := c.PodLogs(t, namespace, apiPod, bootstrapCont)
	adminKey := mustFindKey(t, logs, "scope=admin")
	agentFleetKey := mustFindKey(t, logs, "scope=token")
	t.Logf("captured bootstrap keys (admin prefix=%s, agent-fleet prefix=%s)",
		keyPrefix(adminKey), keyPrefix(agentFleetKey))

	// 4. apply an IdentityMapping CR linked to a Teams surface user.
	const surfaceUser = "aad:aaaa-1111-2222-3333"
	manifest := fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: IdentityMapping
metadata:
  name: alice
  namespace: %s
spec:
  identity:
    kind: user
    displayName: Alice Wong
  bindings:
    - provider: teams
      type: user
      externalID: %s
`, namespace, surfaceUser)
	manifestPath := filepath.Join(t.TempDir(), "identitymapping.yaml")
	mustWriteFile(t, manifestPath, manifest)
	c.ApplyAndWait(t, manifestPath, namespace,
		"identitymapping.platform.iterabase.com/alice",
		"jsonpath={.status.ready}=true", 60*time.Second)

	// 5. reach the api via port-forward.
	base, _ := c.PortForward(t, namespace, apiService, apiPort, 18080)
	client := kindtest.HTTPClient()

	// 6. JWKS -> 200 + >=1 RSA key.
	jwksBody := mustGet(t, client, base+"/.well-known/jwks.json", "")
	jwks, err := kindtest.ParseJWKS([]byte(jwksBody))
	if err != nil {
		t.Fatalf("parse jwks: %v\nbody: %s", err, jwksBody)
	}
	if len(jwks.Keys) == 0 {
		t.Fatalf("jwks has no keys: %s", jwksBody)
	}

	// 7. delegated token (Path 2): agent-fleet SA key + linked surface user.
	token := mustPostToken(t, client, base+"/v1/token", agentFleetKey, surfaceUser)
	pub, err := jwks.PublicKey("")
	if err != nil {
		t.Fatalf("jwks public key: %v", err)
	}
	payload, err := kindtest.VerifyRS256(token, pub)
	if err != nil {
		t.Fatalf("jwt signature verify failed: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
		Iss string `json:"iss"`
		Aud string `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("parse jwt claims: %v", err)
	}
	if claims.Sub == "" {
		t.Errorf("jwt sub claim is empty: %s", payload)
	}
	t.Logf("issued JWT sub=%s iss=%s aud=%s", claims.Sub, claims.Iss, claims.Aud)

	// 8. admin CRUD smoke (optional): create a local user with the admin key.
	//    Kept minimal — the control-plane repo's integration tests cover CRUD.
	createUser(t, client, base+"/v1/users", adminKey, "e2e-bob@local")

	// 9. delete the IdentityMapping -> the identity is soft-deleted; a token for
	//    that user must now be denied (403, enrolled mode: unlinked -> denied).
	c.Kubectl(t, "delete", "-n", namespace, "identitymapping.platform.iterabase.com", "alice")
	assertTokenStatus(t, client, base+"/v1/token", agentFleetKey, surfaceUser, http.StatusForbidden)
}

// --- helpers ---

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

var keyRe = regexp.MustCompile(`API key \(([^)]+)\): (\S+)`)

// mustFindKey parses a bootstrap key of the given scope annotation from
// bootstrap logs. scope is the literal text inside the parens, e.g.
// "scope=admin" or "scope=token".
func mustFindKey(t *testing.T, logs, scope string) string {
	t.Helper()
	for _, m := range keyRe.FindAllStringSubmatch(logs, -1) {
		if m[1] == scope {
			return m[2]
		}
	}
	t.Fatalf("bootstrap key %q not found in logs:\n%s", scope, logs)
	return ""
}

func keyPrefix(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:8]
}

// mustGet performs a GET and returns the body, failing on non-2xx. authHeader
// is "" for unauthenticated requests.
func mustGet(t *testing.T, client *http.Client, url, authHeader string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", "Bearer "+authHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("get %s: status %d\n%s", url, resp.StatusCode, body)
	}
	return string(body)
}

// mustPostToken POSTs /v1/token with the given SA key + surface user and returns
// the access_token, failing on non-200.
func mustPostToken(t *testing.T, client *http.Client, url, saKey, surfaceUser string) string {
	t.Helper()
	body := fmt.Sprintf(`{"provider":"teams","type":"user","externalID":%q}`, surfaceUser)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+saKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post %s: status %d\n%s", url, resp.StatusCode, respBody)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(respBody, &tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("token response missing access_token: %s", respBody)
	}
	return tok.AccessToken
}

// assertTokenStatus POSTs /v1/token and asserts the HTTP status (used for the
// soft-delete denial check).
func assertTokenStatus(t *testing.T, client *http.Client, url, saKey, surfaceUser string, want int) {
	t.Helper()
	body := fmt.Sprintf(`{"provider":"teams","type":"user","externalID":%q}`, surfaceUser)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+saKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("post %s after delete: status %d, want %d\n%s", url, resp.StatusCode, want, respBody)
	}
}

// createUser POSTs /v1/users as a minimal admin-CRUD smoke check.
func createUser(t *testing.T, client *http.Client, url, adminKey, email string) {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"role":"user"}`, email)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("create user: status %d\n%s", resp.StatusCode, respBody)
	}
}
