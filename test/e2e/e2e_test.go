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
	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
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

func TestE2E(t *testing.T) {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		t.Skip("DIGITALOCEAN_TOKEN not set; skipping e2e")
	}

	ctx := context.Background()
	client := godo.NewFromToken(token)
	runID := fmt.Sprintf("forge-e2e-%d", time.Now().Unix())
	keep := os.Getenv("FORGE_E2E_KEEP") != ""

	t.Logf("run %s (keep=%v)", runID, keep)

	// 1. ephemeral SSH keypair
	pubKeyStr, privKeyPath := generateKey(t)

	// 2. create droplet with cloud-init forge user. Provisioning is retried
	//    because DO droplets occasionally never accept SSH within a reasonable
	//    window (boot/cloud-init variance in fra1); a fresh droplet is cheaper
	//    than failing the whole run. The successful droplet is cleaned up by the
	//    defer; failed attempts are deleted inline (the reaper is the safety net).
	var (
		ip      string
		droplet *godo.Droplet
	)
	const maxDropletAttempts = 2
	for attempt := 1; attempt <= maxDropletAttempts; attempt++ {
		d, err := createDroplet(ctx, client, runID, pubKeyStr)
		if err != nil {
			t.Logf("create droplet attempt %d/%d failed: %v", attempt, maxDropletAttempts, err)
			if attempt < maxDropletAttempts {
				time.Sleep(5 * time.Second)
				continue
			}
			t.Fatalf("create droplet failed after %d attempts: %v", maxDropletAttempts, err)
		}
		attemptIP, err := waitForIP(ctx, client, d.ID)
		if err != nil {
			t.Logf("wait for IP attempt %d/%d failed: %v", attempt, maxDropletAttempts, err)
			deleteDroplet(ctx, client, d.ID)
			if attempt < maxDropletAttempts {
				continue
			}
			t.Fatalf("wait for IP failed after %d attempts: %v", maxDropletAttempts, err)
		}
		t.Logf("droplet ip %s (attempt %d/%d)", attemptIP, attempt, maxDropletAttempts)
		if err := waitForHostReady(ctx, attemptIP, privKeyPath); err != nil {
			t.Logf("host readiness attempt %d/%d failed: %v", attempt, maxDropletAttempts, err)
			deleteDroplet(ctx, client, d.ID)
			if attempt < maxDropletAttempts {
				continue
			}
			t.Fatalf("host never became ready after %d droplet attempts: %v", maxDropletAttempts, err)
		}
		ip, droplet = attemptIP, d
		break
	}
	defer func() {
		if droplet == nil {
			return
		}
		if keep {
			t.Logf("keeping droplet %d (run %s) for debugging", droplet.ID, runID)
			return
		}
		deleteDroplet(ctx, client, droplet.ID)
	}()

	// On any failure, dump pods + events (via SSH + the on-host k3s kubeconfig)
	// before the droplet is destroyed - helm's --wait timeout error is terse, so
	// this shows which pod was pending/crashlooping.
	defer func() {
		if !t.Failed() {
			return
		}
		sc, err := sshDial(ip, privKeyPath)
		if err != nil {
			t.Logf("debug pod dump: ssh dial %s failed: %v", ip, err)
			return
		}
		defer sc.Close()
		out, _ := sshOutput(sc, "sudo kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get pods -A 2>&1")
		t.Logf("debug pod dump (on failure):\n%s", out)
		wl, _ := sshOutput(sc, "sudo kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get deploy,daemonset,statefulset,job -A 2>&1")
		t.Logf("debug workloads (deploy/ds/sts/job):\n%s", wl)
		hv, _ := sshOutput(sc, "sudo helm version 2>&1")
		t.Logf("helm version: %s", hv)
		hs, _ := sshOutput(sc, fmt.Sprintf("sudo helm --kubeconfig /etc/rancher/k3s/k3s.yaml status %s -n iterabase-system 2>&1", runID))
		t.Logf("helm status:\n%s", hs)
		ev, _ := sshOutput(sc, "sudo kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get events -A --sort-by=.lastTimestamp 2>&1 | tail -30")
		t.Logf("debug events (tail):\n%s", ev)
	}()

	// 3. build forge binary from the repo root
	forgeBin := buildForge(t)

	// 4. write forge.yaml + set FORGE_HOME to a temp dir. The chart version is
	//    auto-resolved to the latest stable iterabase-platform release (HOR-321)
	//    so the test never drifts from the published charts.
	chartVersion := os.Getenv("ITERABASE_CHART_VERSION")
	if chartVersion == "" {
		chartVersion = kindtest.LatestChartVersion(t, "iterabase-platform")
	}
	forgeHome := t.TempDir()
	cfgPath := writeForgeConfig(t, runID, ip, privKeyPath, chartVersion)

	// 4b. stand up a file:// overlay repo on the host with the MetalLB L2 edge
	//     values (pool = the droplet's public IP). forge apply clones it (file://)
	//     and feeds values.yaml to the chart. The chart defaults (TLS-on, host
	//     gateway.iterabase.local, self-signed) + the overlay's metallb pool bring
	//     up the public HTTPS edge. Mirrors the overlay deployment path OPO1 uses.
	writeEdgeOverlayOnHost(t, ip, privKeyPath)

	// 5. forge apply (k3s + platform chart + overlay via helm). Retried because the k3s
	//    install script's binary download from GitHub releases is prone to
	//    transient DO egress failures; `apply` is idempotent (re-reads live
	//    state and reconciles) so re-running is safe. Only non-zero exits are
	//    retried — a 0-exit missing "node ready: true" is a real regression.
	out := applyWithRetry(t, forgeBin, forgeHome, cfgPath)
	if !strings.Contains(out, "node ready: true") {
		t.Fatalf("apply did not report node ready:\n%s", out)
	}
	if !strings.Contains(out, "chart applied: true") {
		t.Fatalf("apply did not report chart applied:\n%s", out)
	}
	if !strings.Contains(out, "overlay applied: true") {
		t.Fatalf("apply did not report overlay applied:\n%s", out)
	}
	t.Logf("apply output:\n%s", out)

	// 6. node label + Ready + dual-stack pod CIDRs via the off-host kubeconfig (client-go)
	kcPath := filepath.Join(forgeHome, runID, "kubeconfig.yaml")
	checkNodeViaKubeconfig(t, kcPath, runID)

	// 7. gateway pod Running + /health 200 over the real HTTPS edge (LoadBalancer
	//    + MetalLB + cert-manager self-signed TLS), not a hostNetwork HTTP curl.
	checkGatewayRunning(t, kcPath)
	checkGatewayHealth(t, ip)
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

	f, err := os.CreateTemp("", "forge-e2e-key-*")
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := os.WriteFile(f.Name(), privPEM, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	f.Close()
	return pubKeyStr, f.Name()
}

func cloudInit(pubKeyStr string) string {
	return fmt.Sprintf(`#cloud-config
packages: [curl, git]
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

func waitForSSH(ctx context.Context, ip, keyPath string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if _, err := sshDial(ip, keyPath); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("SSH to %s never became reachable", ip)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
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
	for time.Now().Before(end) {
		client, err := sshDial(ip, keyPath)
		if err == nil {
			out, _ := sshOutput(client, "cloud-init status")
			client.Close()
			switch {
			case strings.Contains(out, "status: done"):
				return nil
			case strings.Contains(out, "status: error"):
				return fmt.Errorf("cloud-init failed on %s: %s", ip, strings.TrimSpace(out))
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("host %s never became ready (SSH up + cloud-init done) within %s", ip, deadline)
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
  chart:
    version: %s
  overlay:
    repo: file:///tmp/edge-overlay
    ref: master
`, name, ip, keyPath, name, chartVersion)
	p := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// runForgeE runs forge and returns its combined output and error (no t.Fatalf).
func runForgeE(bin, forgeHome string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "FORGE_HOME="+forgeHome)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// applyWithRetry runs `forge apply` with bounded retries on non-zero exit. The
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
	// Reach the gateway over the real HTTPS edge: Host + SNI = gateway.iterabase.local
	// (the chart default), pinned to the droplet IP - the MetalLB-assigned LoadBalancer
	// IP on this single-node cluster (kube-proxy DNATs <ip>:443 to ingress-nginx).
	// InsecureSkipVerify: the edge uses the self-signed issuer (kind/E2E).
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed e2e cert
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, "443"))
		},
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}
	url := "https://gateway.iterabase.local/health"
	deadline := time.Now().Add(180 * time.Second) // MetalLB LB-IP + cert issuance + ingress sync
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
	t.Fatalf("gateway /health not 200 via %s (ip %s)", url, ip)
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
