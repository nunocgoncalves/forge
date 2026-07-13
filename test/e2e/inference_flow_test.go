package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
)

// TestInferenceFlowContract deploys the iterabase-platform umbrella chart
// (control-plane + inference-gateway + shared pgvector Postgres with the
// read-only `gateway` user + redis) to a local Kind cluster and exercises the
// control-plane↔gateway contract end-to-end via real LISTEN/NOTIFY propagation.
//
// Flow:
//
//	helm install umbrella (control-plane 0.0.6 + gateway 0.2.2, shared PG)
//	  -> capture admin key from the control-plane bootstrap init container
//	  -> apply IdentityMapping CR (the identity — CRD path) -> status.identityID
//	  -> POST /v1/api-keys (admin) -> gateway-scoped key (keys are secret, not CRDs)
//	  -> apply ModelBackend (vLLM) + Model (alias) CRs
//	  -> GET /admin/v1/snapshot (gateway admin key) -> catalog has the alias
//	     (catalog propagation: control-plane DB views -> gateway cache)
//	  -> POST /v1/chat/completions (gateway key) -> 503 (model not available)
//	     proves: auth (key found) + capability (allowed) + catalog (model found)
//	     + available=false (honest — no GPU on kind, the vLLM pod is unschedulable)
//	  -> POST /v1/chat/completions (invalid key) -> 401 (auth denial)
//
// This asserts the contract (catalog + API-key + capability propagation via
// LISTEN/NOTIFY), NOT request→completion — there is no backend on kind, so
// `available` stays false honestly and the gateway returns 503, not a fake
// completion. The real serving path is covered by TestInferenceFlowGPU.
//
// The umbrella chart is published at
// oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform. By default
// the chart version is auto-resolved from the charts repo's latest stable
// GitHub release (HOR-321); the umbrella bakes in the control-plane + gateway
// subcharts at matching image tags, so no image override is needed. Override
// via env for local dev / pinning: ITERABASE_LOCAL_CHART points at a checkout,
// ITERABASE_LOCAL_IMAGE loads a locally-built image into the Kind nodes, and
// ITERABASE_CHART_VERSION pins a specific release.
func TestInferenceFlowContract(t *testing.T) {
	// --- chart config (env-overridable) ---
	chartRef := envOr("ITERABASE_CHART", "oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform")
	localChart := os.Getenv("ITERABASE_LOCAL_CHART")
	chartVersion := os.Getenv("ITERABASE_CHART_VERSION")
	if chartVersion == "" && localChart == "" {
		chartVersion = kindtest.LatestChartVersion(t, "iterabase-platform")
	}

	namespace := "iterabase-system"
	release := "itb"

	// Chart-coupled names (umbrella release prefix "itb").
	const (
		alias          = "e2e-qwen35"
		mbName         = "e2e-backend"
		apiSelector    = "app.kubernetes.io/component=api"
		bootstrapCont  = "bootstrap"
		apiService     = "svc/itb-control-plane-api"
		gatewayService = "svc/itb-gateway"
		gatewaySecret  = "itb-gateway-admin"
		svcPort        = 8080
	)

	// 1. Kind cluster.
	c := kindtest.CreateCluster(t, "forge-inference-e2e")

	if localImage := os.Getenv("ITERABASE_LOCAL_IMAGE"); localImage != "" {
		c.LoadImage(t, localImage)
	}

	// 2. helm install the umbrella. Disable ingress-nginx + minio (not needed on
	//    kind; the test port-forwards the api + gateway directly). redis stays
	//    enabled (the gateway uses it for rate-limit counters).
	values := map[string]string{
		"ingress-nginx.enabled": "false",
		"minio.enabled":         "false",
	}
	c.HelmInstall(t, release, chartRef, chartVersion, namespace, localChart, values)

	// 3. capture the control-plane admin key from the api pod's bootstrap init
	//    container (same pattern as TestControlPlaneIdentity).
	apiPod := c.FirstPodName(t, namespace, apiSelector)
	logs := c.PodLogs(t, namespace, apiPod, bootstrapCont)
	adminKey := mustFindKey(t, logs, "scope=admin")
	t.Logf("captured control-plane admin key (prefix=%s)", keyPrefix(adminKey))

	// 4. port-forward the control-plane API.
	apiBase, _ := c.PortForward(t, namespace, apiService, svcPort, 18080)
	apiClient := kindtest.HTTPClient()

	// 5. apply an IdentityMapping CR (the identity — the CRD path). The
	//    reconciler materializes it into identity.identities + writes
	//    status.identityID (the UUID the API-key endpoint needs).
	imManifest := fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: IdentityMapping
metadata:
  name: inference-user
  namespace: %s
spec:
  identity:
    kind: user
    displayName: Inference E2E User
  bindings:
    - provider: teams
      type: user
      externalID: aad:inference-e2e-user
`, namespace)
	imPath := filepath.Join(t.TempDir(), "identitymapping.yaml")
	mustWriteFile(t, imPath, imManifest)
	c.ApplyAndWait(t, imPath, namespace,
		"identitymapping.platform.iterabase.com/inference-user",
		"jsonpath={.status.ready}=true", 60*time.Second)
	identityID := strings.TrimSpace(c.Kubectl(t, "get", "-n", namespace,
		"identitymapping.platform.iterabase.com", "inference-user",
		"-o", "jsonpath={.status.identityID}"))
	if identityID == "" {
		t.Fatalf("IdentityMapping inference-user has no status.identityID")
	}
	t.Logf("materialized identity %s", identityID)

	// 6. create a gateway-scoped API key (POST /v1/api-keys — keys are secret,
	//    never CRDs). The full key is returned once; only the hash is stored.
	gatewayKey := createAPIKey(t, apiClient, apiBase+"/v1/api-keys", adminKey, identityID, "gateway")
	t.Logf("issued gateway-scoped API key (prefix=%s)", keyPrefix(gatewayKey))

	// 7. apply a ModelBackend (kind: vLLM) + a Model (alias). On kind there is no
	//    GPU, so the vLLM Deployment stays Pending (unschedulable: no
	//    nvidia.com/gpu) -> healthy=false -> available=false (honest). The gateway
	//    returns 503, not a fake completion. This is the same reconciler path the
	//    GPU E2E exercises with a real backend.
	overlayDir := t.TempDir()
	clientDir := filepath.Join(overlayDir, "crds", "client")
	if err := os.MkdirAll(filepath.Join(overlayDir, "crds", "base"), 0o755); err != nil {
		t.Fatalf("mkdir crds/base: %v", err)
	}
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatalf("mkdir crds/client: %v", err)
	}
	// Apply via the overlay's kustomize path (kubectl apply -k crds/client/) —
	// the same mechanism forge uses (HOR-341) — validating the base/client
	// composition renders + applies against the real CRDs.
	mustWriteFile(t, filepath.Join(overlayDir, "crds", "base", "kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nmetadata:\n  name: overlay-crds-base\nresources: []\n")
	mustWriteFile(t, filepath.Join(clientDir, "kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nmetadata:\n  name: overlay-crds-client\nresources:\n  - ../base\n  - modelbackend.yaml\n  - model.yaml\n")
	mustWriteFile(t, filepath.Join(clientDir, "modelbackend.yaml"), fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: ModelBackend
metadata:
  name: %s
  namespace: %s
spec:
  kind: vLLM
  model: Qwen/Qwen3.5-0.8B
`, mbName, namespace))
	mustWriteFile(t, filepath.Join(clientDir, "model.yaml"), fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: Model
metadata:
  name: %s
  namespace: %s
spec:
  modelID: %s
  displayName: E2E Model
  backendRef: %s
  transforms:
    rewrite_model_name: true
`, mbName, namespace, alias, mbName))
	c.Kubectl(t, "apply", "-k", clientDir)
	// Wait for the ModelBackend to be materialized (deployed=true; the Deployment
	// is created even though the pod stays Pending on kind).
	c.Kubectl(t, "wait", "-n", namespace, "modelbackend.platform.iterabase.com/"+mbName,
		"--for", "jsonpath={.status.deployed}=true", "--timeout", "60s")

	// 8. get the gateway's admin key (for /admin/v1/snapshot) from its secret.
	gatewayAdminKey := getSecretKey(t, c, namespace, gatewaySecret, "adminApiKey")

	// 9. port-forward the gateway.
	gwBase, _ := c.PortForward(t, namespace, gatewayService, svcPort, 18081)
	gwClient := kindtest.HTTPClient()

	// 10. poll the gateway's /admin/v1/snapshot until the catalog has the alias.
	//     This is the contract assertion: the control-plane materialized the CRs
	//     into the DB views, and the gateway consumed them via LISTEN/NOTIFY into
	//     its in-memory cache. Real propagation, no polling of the DB.
	entry := waitForCatalogEntry(t, gwClient, gwBase, gatewayAdminKey, alias, 120*time.Second)
	t.Logf("catalog propagated: alias=%s available=%v backend=%s",
		entry.ModelID, entry.Available, entry.BackendURL)

	// 11. assert available=false (honest — no GPU on kind).
	if entry.Available {
		t.Errorf("catalog entry available=true, want false (no GPU on kind; the vLLM pod should be unschedulable)")
	}

	// 12. curl /v1/chat/completions with the gateway key -> 503 (model not
	//     available). Proves the full chain succeeded: auth (key found in the
	//     snapshot) + capability (allowed — v1 broad-default) + catalog (model
	//     found) + available (false, honest). NOT 401/403.
	status, body := chatCompletionsStatus(t, gwClient, gwBase, gatewayKey, alias)
	if status != http.StatusServiceUnavailable {
		t.Errorf("chat completions with gateway key: status %d, want %d (model not available)\n%s", status, http.StatusServiceUnavailable, body)
	}
	t.Logf("gateway key -> %d (model not available, honest)", status)

	// 13. curl with an invalid key -> 401 (auth denial: the key is not in the
	//     snapshot's active_api_keys view).
	status, body = chatCompletionsStatus(t, gwClient, gwBase, "cp-invalid-nonexistent-key", alias)
	if status != http.StatusUnauthorized {
		t.Errorf("chat completions with invalid key: status %d, want %d\n%s", status, http.StatusUnauthorized, body)
	}
	t.Logf("invalid key -> %d (auth denial)", status)
}

// --- inference-flow helpers ---

// catalogEntry mirrors the relevant fields of the gateway's
// /admin/v1/snapshot catalog entries (snapshot.CatalogEntry).
type catalogEntry struct {
	ModelID    string `json:"model_id"`
	Available  bool   `json:"available"`
	BackendURL string `json:"backend_url"`
}

// createAPIKey POSTs /v1/api-keys as the control-plane admin to issue a
// gateway-scoped key for the given identity. Returns the full key (shown once).
func createAPIKey(t *testing.T, client *http.Client, url, adminKey, identityID, scope string) string {
	t.Helper()
	body := fmt.Sprintf(`{"identityID":%q,"name":"e2e-%s","scope":%q}`, identityID, scope, scope)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
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
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create api key: status %d\n%s", resp.StatusCode, respBody)
	}
	var out struct {
		FullKey string `json:"fullKey"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil || out.FullKey == "" {
		t.Fatalf("api-key response missing fullKey: %s", respBody)
	}
	return out.FullKey
}

// getSecretKey reads a key from a Kubernetes Secret via kubectl + base64-decodes
// it (kubectl jsonpath returns the raw base64).
func getSecretKey(t *testing.T, c *kindtest.Cluster, namespace, name, key string) string {
	t.Helper()
	b64 := strings.TrimSpace(c.Kubectl(t, "get", "secret", name, "-n", namespace,
		"-o", fmt.Sprintf("jsonpath={.data.%s}", key)))
	if b64 == "" {
		t.Fatalf("secret %s/%s has no key %q", namespace, name, key)
	}
	dec, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode secret %s/%s[%s]: %v", namespace, name, key, err)
	}
	return string(dec)
}

// snapshotCatalog fetches the gateway's /admin/v1/snapshot (X-Admin-Key auth)
// and returns the catalog entries + the HTTP status. Best-effort: a request or
// parse error returns (nil, 0, err) so callers can keep polling.
func snapshotCatalog(client *http.Client, baseURL, adminKey string) ([]catalogEntry, int, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+"/admin/v1/snapshot", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Admin-Key", adminKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var snap struct {
		Catalog []catalogEntry `json:"catalog"`
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, resp.StatusCode, err
	}
	return snap.Catalog, resp.StatusCode, nil
}

// waitForCatalogEntry polls the gateway's /admin/v1/snapshot until the catalog
// contains the given alias, then returns it. Fails on timeout.
// This is the LISTEN/NOTIFY propagation assertion.
func waitForCatalogEntry(t *testing.T, client *http.Client, baseURL, adminKey, alias string, timeout time.Duration) catalogEntry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		catalog, status, err := snapshotCatalog(client, baseURL, adminKey)
		if err == nil && status == http.StatusOK {
			for _, e := range catalog {
				if e.ModelID == alias {
					return e
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("alias %q never appeared in the gateway snapshot within %s", alias, timeout)
	return catalogEntry{}
}

// chatCompletionsStatus POSTs /v1/chat/completions with the given Bearer key +
// model alias and returns (status, body). It does not fatal on non-2xx — the
// test asserts specific non-200 statuses (503, 401).
func chatCompletionsStatus(t *testing.T, client *http.Client, baseURL, key, model string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post chat completions: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody)
}
