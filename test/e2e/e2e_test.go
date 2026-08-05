// Package e2e runs the forge end-to-end test against an ephemeral
// DigitalOcean droplet. It is a separate module so godo/client-go stay out of
// the main module's dependency graph.
//
// Run: make test-e2e   (requires DIGITALOCEAN_TOKEN)
package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"github.com/nunocgoncalves/forge/test/e2e/internal/runner"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	region  = "fra1"
	size    = "s-2vcpu-4gb" // full stack + MetalLB needs headroom; s-1vcpu-2gb timed out on helm --wait
	image   = "ubuntu-24-04-x64"
	k3sPort = 6443
)

type digitalOceanCPUState struct {
	ctx          context.Context
	client       *godo.Client
	runID        string
	keep         bool
	pubKey       string
	privKeyPath  string
	droplet      *godo.Droplet
	ip           string
	forgeBin     string
	forgeHome    string
	chartVersion string
}

func runDigitalOceanCPU(t *testing.T) {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		t.Skip("DIGITALOCEAN_TOKEN not set; skipping DigitalOcean CPU scenario")
	}

	state := &digitalOceanCPUState{
		ctx:       context.Background(),
		client:    godo.NewFromToken(token),
		runID:     fmt.Sprintf("forge-e2e-%d", time.Now().Unix()),
		keep:      os.Getenv("FORGE_E2E_KEEP") != "",
		forgeHome: t.TempDir(),
	}
	state.pubKey, state.privKeyPath = generateKey(t)
	state.forgeBin = buildForge(t)
	state.chartVersion = platformChartVersion(t, "")
	t.Logf("run %s (keep=%v)", state.runID, state.keep)
	t.Cleanup(func() { state.cleanup(t) })

	runner.RunStages(t, state,
		runner.Stage[*digitalOceanCPUState]{Name: "provision-host", Run: provisionCPUStage},
		runner.Stage[*digitalOceanCPUState]{Name: "reject-gpu-on-cpu-host", Run: rejectGPUOnCPUStage},
		runner.Stage[*digitalOceanCPUState]{Name: "install-migration-source", Run: applyBaselineStage},
		runner.Stage[*digitalOceanCPUState]{Name: "assert-migration-source-edge", Run: assertBaselineStage},
		runner.Stage[*digitalOceanCPUState]{Name: "upgrade-current-with-exact-flux", Run: runOverlayStage},
		runner.Stage[*digitalOceanCPUState]{Name: "assert-current-platform", Run: assertCurrentPlatformStage},
		runner.Stage[*digitalOceanCPUState]{Name: "reapply-current-idempotently", Run: reapplyCurrentPlatformStage},
		runner.Stage[*digitalOceanCPUState]{Name: "sync-secrets", Run: runSecretsStage},
		runner.Stage[*digitalOceanCPUState]{Name: "reconcile-flux", Run: runFluxStage},
	)
}

func provisionCPUStage(t *testing.T, state *digitalOceanCPUState) {
	t.Helper()
	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		d, err := createDroplet(state.ctx, state.client, state.runID, state.pubKey)
		if err != nil {
			t.Logf("create droplet attempt %d/%d failed: %v", attempt, maxAttempts, err)
			if attempt < maxAttempts {
				time.Sleep(5 * time.Second)
				continue
			}
			t.Fatalf("create droplet failed after %d attempts: %v", maxAttempts, err)
		}
		ip, err := waitForIP(state.ctx, state.client, d.ID)
		if err == nil {
			err = waitForHostReady(state.ctx, ip, state.privKeyPath)
		}
		if err == nil {
			state.droplet, state.ip = d, ip
			t.Logf("droplet ip %s (attempt %d/%d)", ip, attempt, maxAttempts)
			return
		}
		t.Logf("host readiness attempt %d/%d failed: %v", attempt, maxAttempts, err)
		deleteDroplet(state.ctx, state.client, d.ID)
	}
	t.Fatalf("host never became ready after %d droplet attempts", maxAttempts)
}

func rejectGPUOnCPUStage(t *testing.T, state *digitalOceanCPUState) {
	t.Helper()
	cfgPath := writeForgeConfigGPU(t, state.runID, state.ip, state.privKeyPath)
	out, err := runForgeE(state.forgeBin, state.forgeHome, "apply", "--config", cfgPath)
	if err == nil {
		t.Fatalf("apply should fail preflight with no NVIDIA GPU:\n%s", out)
	}
	if !strings.Contains(out, "no NVIDIA GPU") {
		t.Fatalf("GPU preflight failure did not explain the missing NVIDIA GPU:\n%s", out)
	}
}

func applyBaselineStage(t *testing.T, state *digitalOceanCPUState) {
	t.Helper()
	writeEdgeOverlayOnHost(t, state.ip, state.privKeyPath)
	out := applyWithRetry(t, state.forgeBin, state.forgeHome,
		writeForgeConfig(t, state.runID, state.ip, state.privKeyPath, certificateMigrationSourceVersion))
	assertApplyMarkers(t, out, "node ready: true", "chart applied: true", "overlay applied: true")
	t.Logf("apply output:\n%s", out)
}

func assertBaselineStage(t *testing.T, state *digitalOceanCPUState) {
	t.Helper()
	kcPath := filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml")
	checkNodeViaKubeconfig(t, kcPath, state.runID)
	checkGatewayRunning(t, kcPath)
	checkGatewayHealth(t, state.ip)
}

func assertCurrentPlatformStage(t *testing.T, state *digitalOceanCPUState) {
	t.Helper()
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()

	assertRemoteHelmChartVersion(t, sc, state.runID, "iterabase-system", state.chartVersion)
	assertRemoteHelmChartVersion(t, sc, state.runID+"-cert-manager", "iterabase-system", state.chartVersion)
	owner := strings.TrimSpace(mustSSHOutput(t, sc,
		`sudo k3s kubectl get crd certificates.cert-manager.io -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}'`))
	wantOwner := state.runID + "-cert-manager"
	if owner != wantOwner {
		t.Fatalf("certificate CRD release owner = %q, want %q", owner, wantOwner)
	}
	ready, revision, digest := pollFluxReady(t, sc, "gitrepository", "overlay", 2*time.Minute)
	if !ready || revision == "" || !isCanonicalSHA256Digest(digest) {
		t.Fatalf("current platform has no exact Ready Flux artifact: ready=%v revision=%q digest=%q", ready, revision, digest)
	}
	mustSSHOutput(t, sc, fmt.Sprintf("sudo k3s kubectl rollout status -n iterabase-system deployment/%s-tool-runner --timeout=300s", state.runID))

	kcPath := filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml")
	checkGatewayRunning(t, kcPath)
	checkGatewayNodePortHealth(t, kcPath, state.ip)
}

func reapplyCurrentPlatformStage(t *testing.T, state *digitalOceanCPUState) {
	t.Helper()
	out := applyWithRetry(t, state.forgeBin, state.forgeHome,
		writeCurrentOverlayForgeConfig(t, state.runID, state.ip, state.privKeyPath, state.chartVersion))
	assertApplyMarkers(t, out, "action:     skip", "node ready: true", "certificate substrate applied: true",
		"chart applied: true", "overlay applied: true", "flux installed: true")
}

func assertRemoteHelmChartVersion(t *testing.T, sc *ssh.Client, release, namespace, want string) {
	t.Helper()
	out := mustSSHOutput(t, sc, fmt.Sprintf("sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml helm get metadata %s -n %s -o json", release, namespace))
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &metadata); err != nil {
		t.Fatalf("decode Helm metadata for %s: %v\n%s", release, err, out)
	}
	if metadata.Version != want {
		t.Fatalf("Helm release %s chart version = %q, want %q", release, metadata.Version, want)
	}
}

func assertApplyMarkers(t *testing.T, out string, markers ...string) {
	t.Helper()
	for _, marker := range markers {
		if !strings.Contains(out, marker) {
			t.Fatalf("apply output missing %q:\n%s", marker, out)
		}
	}
}

func (state *digitalOceanCPUState) cleanup(t *testing.T) {
	t.Helper()
	if state.droplet == nil {
		return
	}
	if t.Failed() {
		state.dumpDiagnostics(t)
	}
	if state.keep {
		t.Logf("keeping droplet %d (run %s) for debugging", state.droplet.ID, state.runID)
		return
	}
	deleteDroplet(state.ctx, state.client, state.droplet.ID)
}

func (state *digitalOceanCPUState) dumpDiagnostics(t *testing.T) {
	t.Helper()
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Logf("debug pod dump: ssh dial %s failed: %v", state.ip, err)
		return
	}
	defer sc.Close()
	out, _ := sshOutput(sc, "sudo kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get pods -A 2>&1")
	t.Logf("debug pod dump (on failure):\n%s", out)
	events, _ := sshOutput(sc, "sudo kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get events -A --sort-by=.lastTimestamp 2>&1 | tail -30")
	t.Logf("debug events (tail):\n%s", events)
}

func generateKey(t *testing.T) (pubKeyStr, privKeyPath string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubSSH, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("new public key: %v", err)
	}
	pubKeyStr = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pubSSH)))

	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	privPEM := pem.EncodeToMemory(block)

	privKeyPath = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(privKeyPath, privPEM, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return pubKeyStr, privKeyPath
}

func cloudInit(pubKeyStr string) string {
	return fmt.Sprintf(`#cloud-config
packages: [curl]
users:
  - name: forge
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
`, pubKeyStr)
}

func createDroplet(ctx context.Context, client *godo.Client, name, pubKeyStr string) (*godo.Droplet, error) {
	req := &godo.DropletCreateRequest{
		Name:     name,
		Region:   region,
		Size:     size,
		UserData: cloudInit(pubKeyStr),
		IPv6:     true,
		Tags:     []string{"forge-e2e", name},
		Image:    godo.DropletCreateImage{Slug: image},
	}
	d, _, err := client.Droplets.Create(ctx, req)
	return d, err
}

// deleteDroplet best-effort deletes a droplet. Failures are swallowed: the
// reaper workflow (reaper.yml) cleans up any orphaned forge-e2e droplets.
func deleteDroplet(ctx context.Context, client *godo.Client, id int) {
	_, _ = client.Droplets.Delete(ctx, id)
}

func waitForIP(ctx context.Context, client *godo.Client, id int) (string, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		d, _, err := client.Droplets.Get(ctx, id)
		if err != nil {
			return "", err
		}
		if d.Status == "active" {
			for _, n := range d.Networks.V4 {
				if n.Type == "public" {
					return n.IPAddress, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("droplet %d never became active with a public IP", id)
		}
		time.Sleep(5 * time.Second)
	}
}

// waitForHostReady waits until the droplet accepts SSH AND cloud-init has
// finished applying its user-data (the forge user, passwordless sudo, curl).
// Returning only once cloud-init reports "done" prevents forge's preflight from
// racing cloud-init — e.g. `sudo -n true` failing with "passwordless sudo
// required" before the sudoers rule is applied, or curl being absent before
// `packages: [curl]` completes. The forge user is created by cloud-init, so SSH
// can only succeed once cloud-init has at least started.
func waitForHostReady(ctx context.Context, ip, keyPath string) error {
	const deadline = 5 * time.Minute
	end := time.Now().Add(deadline)
	var lastStatus string
	var lastErr error
	for time.Now().Before(end) {
		client, err := sshDial(ip, keyPath)
		if err == nil {
			out, statusErr := sshOutput(client, "cloud-init status")
			client.Close()
			lastStatus, lastErr = strings.TrimSpace(out), statusErr
			switch {
			case strings.Contains(out, "status: done"):
				return nil
			case strings.Contains(out, "status: error"):
				return fmt.Errorf("cloud-init failed on %s: %s", ip, lastStatus)
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("host %s never became ready (SSH up + cloud-init done) within %s (last status %q, last error %v)", ip, deadline, lastStatus, lastErr)
}

// sshOutput runs a command over an SSH client and returns its combined output.
func sshOutput(client *ssh.Client, cmd string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

func sshDial(ip, keyPath string) (*ssh.Client, error) {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            "forge",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // ephemeral test droplet
		Timeout:         10 * time.Second,
	}
	return ssh.Dial("tcp", ip+":22", cfg)
}

func buildForge(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Join(wd, "..", "..")
	bin := filepath.Join(t.TempDir(), "forge")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/forge")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build forge: %v\n%s", err, out)
	}
	return bin
}

func writeForgeConfig(t *testing.T, name, ip, keyPath, chartVersion string) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, RunLabel: true, DualStack: true,
		ChartVersion: chartVersion, OverlayRepo: "file:///tmp/edge-overlay", OverlayRef: "master",
	})
}

// runForgeE runs forge and returns its combined output and error (no t.Fatalf).
func runForgeE(bin, forgeHome string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "FORGE_HOME="+forgeHome)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// applyWithRetryArgs is applyWithRetry with extra forge apply args appended
// (e.g. --skip-chart to isolate a phase that doesn't depend on the platform
// chart). Bounded retries on non-zero exit.
func applyWithRetryArgs(t *testing.T, bin, forgeHome, cfgPath string, extraArgs ...string) string {
	t.Helper()
	const maxAttempts = 3
	var out string
	var lastErr error
	args := append([]string{"apply", "--config", cfgPath}, extraArgs...)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var err error
		out, err = runForgeE(bin, forgeHome, args...)
		if err == nil {
			if attempt > 1 {
				t.Logf("apply succeeded on attempt %d/%d", attempt, maxAttempts)
			}
			return out
		}
		lastErr = err
		t.Logf("apply attempt %d/%d failed: %v\n%s", attempt, maxAttempts, err, out)
		if attempt < maxAttempts {
			time.Sleep(10 * time.Second)
		}
	}
	t.Fatalf("forge apply failed after %d attempts: %v\n%s", maxAttempts, lastErr, out)
	return out
}

// k3s install script's binary download from GitHub releases is prone to
// transient DO egress failures; `apply` is idempotent (re-reads live state and
// reconciles), so re-running after a partial/failed install is safe. A 0-exit
// whose output lacks the expected markers is not retried — that's a real
// regression, not transient infra.
func applyWithRetry(t *testing.T, bin, forgeHome, cfgPath string) string {
	t.Helper()
	const maxAttempts = 3
	var out string
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var err error
		out, err = runForgeE(bin, forgeHome, "apply", "--config", cfgPath)
		if err == nil {
			if attempt > 1 {
				t.Logf("apply succeeded on attempt %d/%d", attempt, maxAttempts)
			}
			return out
		}
		lastErr = err
		t.Logf("apply attempt %d/%d failed: %v\n%s", attempt, maxAttempts, err, out)
		if attempt < maxAttempts {
			time.Sleep(10 * time.Second)
		}
	}
	t.Fatalf("forge apply failed after %d attempts: %v\n%s", maxAttempts, lastErr, out)
	return out
}

func checkNodeViaKubeconfig(t *testing.T, kcPath, wantLabelValue string) {
	t.Helper()
	restCfg, err := clientcmd.BuildConfigFromFlags("", kcPath)
	if err != nil {
		t.Fatalf("build kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("new clientset: %v", err)
	}

	// Poll briefly: the node and its pod CIDR assignment can lag "Ready" slightly.
	var node corev1.Node
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		nodes, lerr := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		if lerr == nil && len(nodes.Items) == 1 {
			node = nodes.Items[0]
			if len(node.Spec.PodCIDRs) > 0 {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if node.Name == "" {
		t.Fatalf("no node found via kubeconfig")
	}

	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		t.Errorf("node %s is not Ready", node.Name)
	}
	if got := node.Labels["e2e.horizonshift.io/run"]; got != wantLabelValue {
		t.Errorf("node label e2e.horizonshift.io/run = %q, want %q", got, wantLabelValue)
	}
	// Dual-stack proof: the node must have an IPv6 pod CIDR.
	hasV6 := false
	for _, c := range node.Spec.PodCIDRs {
		ip := net.ParseIP(strings.SplitN(c, "/", 2)[0])
		if ip != nil && ip.To4() == nil {
			hasV6 = true
		}
	}
	if !hasV6 {
		t.Errorf("node has no IPv6 pod CIDR (dual-stack not active): %v", node.Spec.PodCIDRs)
	}
}

func checkGatewayRunning(t *testing.T, kcPath string) {
	t.Helper()
	restCfg, err := clientcmd.BuildConfigFromFlags("", kcPath)
	if err != nil {
		t.Fatalf("build kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("new clientset: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		pods, lerr := cs.CoreV1().Pods("iterabase-system").List(context.Background(), metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=inference-gateway",
		})
		if lerr == nil && len(pods.Items) > 0 && pods.Items[0].Status.Phase == corev1.PodRunning {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("inference-gateway pod not Running in iterabase-system")
}

func checkGatewayHealth(t *testing.T, ip string) {
	t.Helper()
	// The migration source proves the real MetalLB edge on 443.
	checkGatewayHealthOnPort(t, ip, 443)
}

func checkGatewayNodePortHealth(t *testing.T, kcPath, ip string) {
	t.Helper()
	restCfg, err := clientcmd.BuildConfigFromFlags("", kcPath)
	if err != nil {
		t.Fatalf("build kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("new clientset: %v", err)
	}
	services, err := cs.CoreV1().Services("iterabase-system").List(context.Background(), metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller",
	})
	if err != nil || len(services.Items) != 1 {
		t.Fatalf("resolve ingress controller Service: count=%d err=%v", len(services.Items), err)
	}
	for _, port := range services.Items[0].Spec.Ports {
		if port.Port == 443 && port.NodePort > 0 {
			checkGatewayHealthOnPort(t, ip, int(port.NodePort))
			return
		}
	}
	t.Fatalf("ingress controller Service has no HTTPS NodePort: %+v", services.Items[0].Spec.Ports)
}

func checkGatewayHealthOnPort(t *testing.T, ip string, port int) {
	t.Helper()
	// Reach the gateway over the real HTTPS ingress with the chart's default Host
	// and SNI. The E2E edge uses a self-signed issuer.
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed e2e cert
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, fmt.Sprintf("%d", port)))
		},
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}
	url := "https://gateway.iterabase.local/health"
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("gateway /health not 200 via %s (ip %s port %d)", url, ip, port)
}

// writeEdgeOverlayOnHost creates a file:// overlay git repo on the droplet with
// the MetalLB L2 edge values (IPAddressPool = the droplet's public IP). forge
// apply clones it (file://, tokenless) and feeds values.yaml to the platform
// chart. git is pre-installed by cloud-init. The scaffold matches what forge
// validates: values.yaml + values.client.yaml + crds/client/kustomization.yaml.
func writeEdgeOverlayOnHost(t *testing.T, ip, keyPath string) {
	t.Helper()
	sc, err := sshDial(ip, keyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", ip, err)
	}
	defer sc.Close()
	script := fmt.Sprintf(`set -e
# git is needed to init the overlay repo; install it if absent (mirrors forge's
# EnsureGit). cloud-init only installs curl, so git may not be present yet.
if ! command -v git >/dev/null 2>&1; then
  sudo apt-get update -qq && sudo apt-get install -y git
fi
d=/tmp/edge-overlay
rm -rf "$d"
mkdir -p "$d/crds/client"
cat > "$d/values.yaml" <<'YAML'
metallb:
  enabled: true
metallb-config:
  enabled: true
  addresses:
    - %s-%s
YAML
cat > "$d/values.client.yaml" <<'YAML'
# client-specific overrides (none for e2e)
YAML
cat > "$d/crds/client/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
YAML
cd "$d"
git init -q -b master
git add .
git -c user.email=forge@e2e -c user.name=forge commit -qm init
`, ip, ip)
	if out, err := sshOutput(sc, script); err != nil {
		t.Fatalf("write edge overlay on host: %v\n%s", err, out)
	}
}
