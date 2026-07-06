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
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	region  = "fra1"
	size    = "s-1vcpu-2gb"
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

	// 2. create droplet with cloud-init forge user
	droplet, err := createDroplet(ctx, client, runID, pubKeyStr)
	if err != nil {
		t.Fatalf("create droplet: %v", err)
	}
	defer func() {
		if keep {
			t.Logf("keeping droplet %d (run %s) for debugging", droplet.ID, runID)
			return
		}
		if _, err := client.Droplets.Delete(ctx, droplet.ID); err != nil {
			t.Logf("warning: failed to delete droplet %d: %v (reaper will clean up)", droplet.ID, err)
		}
	}()

	ip, err := waitForIP(ctx, client, droplet.ID)
	if err != nil {
		t.Fatalf("wait for IP: %v", err)
	}
	t.Logf("droplet ip %s", ip)

	if err := waitForSSH(ctx, ip, privKeyPath); err != nil {
		t.Fatalf("wait for SSH: %v", err)
	}

	// 3. build forge binary from the repo root
	forgeBin := buildForge(t)

	// 4. write forge.yaml + set FORGE_HOME to a temp dir
	forgeHome := t.TempDir()
	cfgPath := writeForgeConfig(t, runID, ip, privKeyPath)

	// 5. forge apply
	out := runForge(t, forgeBin, forgeHome, "apply", "--config", cfgPath)
	if !strings.Contains(out, "node ready: true") {
		t.Fatalf("apply did not report node ready:\n%s", out)
	}
	t.Logf("apply output:\n%s", out)

	// 6. dual-stack CIDRs present in k3s config
	remoteCfg := sshRun(t, ip, privKeyPath, "sudo cat /etc/rancher/k3s/config.yaml")
	if !strings.Contains(remoteCfg, "fd42::/48") {
		t.Errorf("k3s config missing IPv6 cluster CIDR:\n%s", remoteCfg)
	}

	// 7. node label present + Ready via the off-host kubeconfig (client-go)
	kcPath := filepath.Join(forgeHome, runID, "kubeconfig.yaml")
	checkNodeViaKubeconfig(t, kcPath, runID)
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

func sshRun(t *testing.T, ip, keyPath, cmd string) string {
	t.Helper()
	client, err := sshDial(ip, keyPath)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("ssh session: %v", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		t.Fatalf("ssh run %q: %v\n%s", cmd, err, out)
	}
	return string(out)
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

func writeForgeConfig(t *testing.T, name, ip, keyPath string) string {
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
`, name, ip, keyPath, name)
	p := filepath.Join(t.TempDir(), "forge.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func runForge(t *testing.T, bin, forgeHome string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "FORGE_HOME="+forgeHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("forge %v: %v\n%s", args, err, out)
	}
	return string(out)
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
	nodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes.Items) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes.Items))
	}
	node := nodes.Items[0]
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
}
