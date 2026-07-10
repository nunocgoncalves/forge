// Package e2e inference-flow GPU scenario: a full request→completion happy
// path on a real GPU droplet — forge bootstraps k3s + the GPU operator + the
// iterabase-platform chart, the control-plane deploys a real vLLM backend, and
// a curl to the gateway with an API key returns a real completion.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
	"github.com/stretchr/testify/require"
)

// TestInferenceFlowGPU runs the full inference happy path on the cheapest
// creatable GPU droplet:
//
//	forge apply (k3s + GPU operator + iterabase-platform umbrella chart)
//	  -> apply ModelBackend(kind: vLLM, Qwen/Qwen3.5-0.8B) + Model (alias)
//	  -> the control-plane deploys vLLM (downloads the model, serves on /health)
//	  -> apply IdentityMapping CR (identity) + POST /v1/api-keys (gateway key)
//	  -> wait for the gateway snapshot to mark the model available (vLLM ready)
//	  -> curl /v1/chat/completions with the gateway key -> a real completion
//
// Skips loudly when no GPU capacity is available so DO scarcity doesn't block
// PRs (same trigger policy as TestGPUE2E). The contract-propagation layer is
// covered by TestInferenceFlowContract on Kind; this test proves real serving.
func TestInferenceFlowGPU(t *testing.T) {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		t.Skip("DIGITALOCEAN_TOKEN not set; skipping e2e")
	}
	ctx := context.Background()
	client := godo.NewFromToken(token)
	runID := fmt.Sprintf("forge-inf-%d", time.Now().Unix())

	// 1. provision the cheapest creatable GPU droplet (skip-loudly on no capacity).
	pubKeyStr, privKeyPath := generateKey(t)
	prov := &doGPUVMProvisioner{client: client}
	vm, err := prov.Provision(ctx, runID, pubKeyStr, privKeyPath)
	if errors.Is(err, ErrNoGPUCapacity) {
		t.Skipf("inference GPU e2e skipped — no GPU capacity (try later or add Verda): %v", err)
	}
	require.NoError(t, err)
	defer func() { _ = prov.Destroy(ctx, vm.ID) }()
	t.Logf("gpu vm ip %s", vm.IP)

	// 2. forge apply: k3s + GPU operator + the iterabase-platform umbrella chart.
	//    The chart version is auto-resolved to the latest stable release so the
	//    test never drifts from the published charts (HOR-321).
	chartVersion := os.Getenv("ITERABASE_CHART_VERSION")
	if chartVersion == "" {
		chartVersion = kindtest.LatestChartVersion(t, "iterabase-platform")
	}
	forgeBin := buildForge(t)
	forgeHome := t.TempDir()
	cfgPath := writeForgeConfigInferenceGPU(t, runID, vm.IP, privKeyPath, chartVersion)
	out := applyWithRetry(t, forgeBin, forgeHome, cfgPath)
	if !strings.Contains(out, "gpu ready: true") {
		dumpGPUDiagnostics(t, vm.IP, privKeyPath)
		t.Fatalf("forge apply did not reach gpu ready:\n%s", out)
	}
	if !strings.Contains(out, "chart applied: true") {
		t.Fatalf("forge apply did not report chart applied:\n%s", out)
	}
	t.Logf("apply output:\n%s", out)

	// 3. wrap the fetched kubeconfig with the kindtest helpers (kubectl +
	//    port-forward against the remote k3s cluster, no Kind cluster created).
	kcPath := filepath.Join(forgeHome, runID, "kubeconfig.yaml")
	c := kindtest.UseCluster(t, runID, kcPath)
	namespace := "iterabase-system"
	const (
		alias       = "qwen35"
		mbName      = "qwen35-backend"
		release     = "itb"
		apiService  = "svc/itb-control-plane-api"
		apiSelector = "app.kubernetes.io/component=api"
		svcPort     = 8080
	)

	// 4. apply a ModelBackend (vLLM, Qwen/Qwen3.5-0.8B) + a Model (alias). The
	//    control-plane deploys vLLM on the GPU node (requests nvidia.com/gpu: 1,
	//    downloads the model, serves on /health).
	catManifest := fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: ModelBackend
metadata:
  name: %s
  namespace: %s
spec:
  kind: vLLM
  model: Qwen/Qwen3.5-0.8B
---
apiVersion: platform.iterabase.com/v1alpha1
kind: Model
metadata:
  name: %s
  namespace: %s
spec:
  modelID: %s
  displayName: Qwen3.5 0.8B
  backendRef: %s
  transforms:
    rewrite_model_name: true
`, mbName, namespace, mbName, namespace, alias, mbName)
	catPath := filepath.Join(t.TempDir(), "catalog.yaml")
	require.NoError(t, os.WriteFile(catPath, []byte(catManifest), 0o600))
	c.Kubectl(t, "apply", "-f", catPath, "-n", namespace)

	// 5. capture the control-plane admin key (bootstrap init container) + apply
	//    an IdentityMapping CR (the identity — CRD path).
	apiPod := c.FirstPodName(t, namespace, apiSelector)
	logs := c.PodLogs(t, namespace, apiPod, "bootstrap")
	adminKey := mustFindKey(t, logs, "scope=admin")
	t.Logf("captured control-plane admin key (prefix=%s)", keyPrefix(adminKey))

	imManifest := fmt.Sprintf(`apiVersion: platform.iterabase.com/v1alpha1
kind: IdentityMapping
metadata:
  name: inference-user
  namespace: %s
spec:
  identity:
    kind: user
    displayName: Inference GPU E2E User
  bindings:
    - provider: teams
      type: user
      externalID: aad:inference-gpu-user
`, namespace)
	imPath := filepath.Join(t.TempDir(), "identitymapping.yaml")
	require.NoError(t, os.WriteFile(imPath, []byte(imManifest), 0o600))
	c.ApplyAndWait(t, imPath, namespace,
		"identitymapping.platform.iterabase.com/inference-user",
		"jsonpath={.status.ready}=true", 60*time.Second)
	identityID := strings.TrimSpace(c.Kubectl(t, "get", "-n", namespace,
		"identitymapping.platform.iterabase.com", "inference-user",
		"-o", "jsonpath={.status.identityID}"))
	require.NotEmpty(t, identityID, "IdentityMapping has no status.identityID")

	// 6. port-forward the control-plane API + issue a gateway-scoped API key.
	apiBase, _ := c.PortForward(t, namespace, apiService, svcPort, 18080)
	apiClient := kindtest.HTTPClient()
	gatewayKey := createAPIKey(t, apiClient, apiBase+"/v1/api-keys", adminKey, identityID, "gateway")
	t.Logf("issued gateway-scoped API key (prefix=%s)", keyPrefix(gatewayKey))

	// 7. get the gateway's admin key + port-forward the gateway. Port-forward
	//    (not the droplet IP / ingress) so the readiness poll + the completion
	//    request depend only on the gateway pod being up — not on ingress-nginx
	//    scheduling on the GPU node (which can lag or be tainted differently).
	gatewayAdminKey := getSecretKey(t, c, namespace, release+"-gateway-admin", "adminApiKey")
	gwBase, _ := c.PortForward(t, namespace, "svc/"+release+"-gateway", svcPort, 18081)
	gwClient := &http.Client{Timeout: 300 * time.Second} // long timeout for inference

	// 8. wait for vLLM to be ready: the gateway snapshot marks the model
	//    available (the control-plane's ModelBackend reconciler sets healthy=true
	//    once the vLLM Deployment is Available — model downloaded + serving).
	entry, ok := waitForModelAvailable(t, c, namespace, mbName, gwClient, gwBase, gatewayAdminKey, alias, 25*time.Minute)
	if !ok {
		dumpVLLMDiagnostics(t, c, namespace, mbName)
		t.Fatalf("model %q never became available within 25m (vLLM pod not healthy; see diagnostics above)", alias)
	}
	t.Logf("model available: alias=%s backend=%s", entry.ModelID, entry.BackendURL)

	// 9. curl /v1/chat/completions with the gateway key -> a real completion.
	status, body := chatCompletionsStatus(t, gwClient, gwBase, gatewayKey, alias)
	if status != http.StatusOK {
		dumpVLLMDiagnostics(t, c, namespace, mbName)
		t.Fatalf("chat completions: status %d, want 200 (real completion)\n%s", status, body)
	}
	content := extractCompletion(body)
	if content == "" {
		t.Fatalf("completion response has no content:\n%s", body)
	}
	t.Logf("real completion (%s): %q", alias, truncate(content, 120))
}

// writeForgeConfigInferenceGPU writes a forge.yaml for the inference GPU e2e:
// single-node k3s + GPU operator + the iterabase-platform umbrella chart.
func writeForgeConfigInferenceGPU(t *testing.T, name, ip, keyPath, chartVersion string) string {
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
  k3s:
    version: v1.31.5+k3s1
    clusterCIDR: 10.42.0.0/16
    serviceCIDR: 10.43.0.0/16
    dualStack: false
  gpu:
    enabled: true
  chart:
    version: %s
    release: itb
    namespace: iterabase-system
`, name, ip, keyPath, chartVersion)
	p := filepath.Join(t.TempDir(), "forge.yaml")
	require.NoError(t, os.WriteFile(p, []byte(cfg), 0o600))
	return p
}

// waitForModelAvailable polls the gateway's /admin/v1/snapshot until the given
// alias is present AND available=true (vLLM ready). The generous timeout covers
// the vLLM image pull + model download + startup on the GPU droplet. Logs the
// last status + body periodically, and dumps vLLM pod diagnostics once at the
// 5m mark (so a crash/image-pull issue is visible without waiting the full
// timeout). Returns (entry, false) on timeout so the caller can dump final
// diagnostics before failing.
func waitForModelAvailable(t *testing.T, c *kindtest.Cluster, namespace, mbName string, client *http.Client, baseURL, adminKey, alias string, timeout time.Duration) (catalogEntry, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus int
	var lastBody string
	nextLog := time.Now().Add(2 * time.Minute)
	earlyDiag := time.Now().Add(5 * time.Minute)
	dumpedEarly := false
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, baseURL+"/admin/v1/snapshot", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Admin-Key", adminKey)
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastStatus = resp.StatusCode
			lastBody = string(body)
			if resp.StatusCode == http.StatusOK {
				var snap struct {
					Catalog []catalogEntry `json:"catalog"`
				}
				if json.Unmarshal(body, &snap) == nil {
					for _, e := range snap.Catalog {
						if e.ModelID == alias && e.Available {
							return e, true
						}
					}
				}
			}
		}
		if time.Now().After(nextLog) {
			t.Logf("waiting for %s: last status=%d body=%.200s", alias, lastStatus, lastBody)
			nextLog = time.Now().Add(2 * time.Minute)
		}
		// Early diagnostics at 5m: if the model is still unavailable, dump the
		// vLLM pod state + logs so the cause is visible without waiting 25m.
		if !dumpedEarly && time.Now().After(earlyDiag) {
			dumpedEarly = true
			t.Logf("model %s still unavailable after 5m; dumping vLLM diagnostics", alias)
			dumpVLLMDiagnostics(t, c, namespace, mbName)
		}
		time.Sleep(15 * time.Second)
	}
	return catalogEntry{}, false
}

// dumpVLLMDiagnostics queries the vLLM backend state when the model never
// becomes available, so the cause (unschedulable nodeSelector, image pull,
// crash, slow startup) is visible in the test log rather than just "not ready".
func dumpVLLMDiagnostics(t *testing.T, c *kindtest.Cluster, namespace, mbName string) {
	t.Helper()
	t.Log("=== vLLM backend diagnostics ===")
	label := "platform.iterabase.com/modelbackend=" + mbName
	t.Logf("$ kubectl get modelbackend %s -o yaml", mbName)
	t.Log(c.Kubectl(t, "get", "modelbackend", mbName, "-n", namespace, "-o", "yaml"))
	t.Logf("$ kubectl get deployment %s -o wide", mbName)
	t.Log(c.Kubectl(t, "get", "deployment", mbName, "-n", namespace, "-o", "wide"))
	t.Logf("$ kubectl get pods -l %s -o wide", label)
	t.Log(c.Kubectl(t, "get", "pods", "-n", namespace, "-l", label, "-o", "wide"))
	t.Logf("$ kubectl describe pod -l %s", label)
	t.Log(c.Kubectl(t, "describe", "pod", "-n", namespace, "-l", label))
	t.Log("$ kubectl get nodes --show-labels (nvidia labels?)")
	t.Log(c.Kubectl(t, "get", "nodes", "--show-labels"))
	pod := strings.TrimSpace(c.Kubectl(t, "get", "pods", "-n", namespace, "-l", label,
		"-o", "jsonpath={.items[0].metadata.name}"))
	if pod != "" {
		t.Logf("$ kubectl logs %s (vLLM startup / model download)", pod)
		t.Log(c.PodLogs(t, namespace, pod, ""))
	}
	t.Log("$ kubectl get events -n " + namespace + " (sorted by time)")
	t.Log(c.Kubectl(t, "get", "events", "-n", namespace, "--sort-by=.lastTimestamp"))
}

// extractCompletion pulls the text content from an OpenAI chat-completion
// response (choices[0].message.content).
func extractCompletion(body string) string {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return ""
	}
	if len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].Message.Content
}

// truncate clips a string to n chars for log lines.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
