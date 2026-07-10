// Package kindtest is a reusable harness for forge's chart-wiring e2e tests on
// a local Kind cluster. It is deliberately substrate-agnostic: it does not run
// forge or bootstrap k3s (that path is covered by the DigitalOcean e2e, which
// tests forge itself). This harness exists to validate deployed charts and the
// HTTP/CR flows on top of them.
//
// It is intended as the forge e2e pattern for future control-plane CRDs
// (PermissionPolicy / Model / ModelBackend / AgentSandbox): each test creates a
// cluster, installs its chart, and exercises its flow with the helpers below.
package kindtest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Cluster is a handle to a running Kind cluster bound to a test.
type Cluster struct {
	Name       string
	Kubeconfig string // absolute path to the kubeconfig for this cluster
	t          *testing.T
}

// UseCluster wraps an existing kubeconfig (e.g. fetched from a forge-
// provisioned cluster) with the Cluster helpers, without creating or deleting
// a cluster. Used by the GPU e2e, which reuses the kubectl/port-forward helpers
// against a remote k3s cluster instead of a local Kind cluster.
func UseCluster(t *testing.T, name, kubeconfig string) *Cluster {
	t.Helper()
	return &Cluster{Name: name, Kubeconfig: kubeconfig, t: t}
}

// CreateCluster provisions a Kind cluster named `name` and registers cleanup to
// delete it. It fails the test if `kind` is not on PATH. The kubeconfig is
// written to a temp path so the cluster is fully isolated from the operator's
// ~/.kube/config.
func CreateCluster(t *testing.T, name string) *Cluster {
	t.Helper()
	mustBin(t, "kind")
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	run(t, "kind", "create", "cluster", "--name", name, "--kubeconfig", kubeconfig)
	t.Cleanup(func() {
		// best-effort teardown; never fail the test during cleanup
		_ = exec.Command("kind", "delete", "cluster", "--name", name).Run()
	})
	return &Cluster{Name: name, Kubeconfig: kubeconfig, t: t}
}

// LoadImage loads a locally-built image into the Kind nodes (for dev against an
// unreleased build). No-op equivalent of `kind load docker-image`.
func (c *Cluster) LoadImage(t *testing.T, image string) {
	t.Helper()
	run(t, "kind", "load", "docker-image", "--name", c.Name, image)
}

// HelmInstall runs `helm upgrade --install` and waits for resources to be ready.
// If localChart is non-empty it is used as the chart path (dev against a
// checkout); otherwise chartRef is used (an OCI ref or repo/name) with --version.
// values is a flat map of --set key=value pairs.
func (c *Cluster) HelmInstall(t *testing.T, release, chartRef, version, namespace, localChart string, values map[string]string) {
	t.Helper()
	mustBin(t, "helm")
	args := []string{
		"upgrade", "--install", release,
		"--namespace", namespace, "--create-namespace",
		"--kubeconfig", c.Kubeconfig,
		"--wait", "--timeout", "5m",
	}
	if localChart != "" {
		args = append(args, localChart)
	} else {
		args = append(args, chartRef)
		if version != "" {
			args = append(args, "--version", version)
		}
	}
	for k, v := range values {
		args = append(args, "--set", k+"="+v)
	}
	run(t, "helm", args...)
}

// Kubectl runs kubectl against the cluster and returns combined stdout.
func (c *Cluster) Kubectl(t *testing.T, args ...string) string {
	t.Helper()
	mustBin(t, "kubectl")
	full := append([]string{"--kubeconfig", c.Kubeconfig}, args...)
	return run(t, "kubectl", full...)
}

// KubectlE runs kubectl and returns (output, error) without fataling — for
// best-effort diagnostics where a failing command shouldn't abort the rest
// (mirrors dumpGPUDiagnostics: log the error, keep going).
func (c *Cluster) KubectlE(t *testing.T, args ...string) (string, error) {
	t.Helper()
	mustBin(t, "kubectl")
	full := append([]string{"--kubeconfig", c.Kubeconfig}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	return string(out), err
}

// FirstPodName returns the name of the first pod matching a label selector
// (e.g. "app.kubernetes.io/component=api"), polling briefly until one exists.
func (c *Cluster) FirstPodName(t *testing.T, namespace, selector string) string {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out := c.Kubectl(t, "get", "pods", "-n", namespace, "-l", selector,
			"-o", `jsonpath={.items[0].metadata.name}`)
		if strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no pod found in %s with selector %q", namespace, selector)
	return ""
}

// PodLogs returns the logs of a container in the named pod. Pass container==""
// for the default container; pass an init-container name to read bootstrap /
// migrate output.
func (c *Cluster) PodLogs(t *testing.T, namespace, pod, container string) string {
	t.Helper()
	args := []string{"logs", "-n", namespace, pod}
	if container != "" {
		args = append(args, "-c", container)
	}
	return c.Kubectl(t, args...)
}

// ApplyAndWait applies a manifest file and then waits for a condition on a
// resource. resource is a fully-qualified "kind.group/name" (or "kind/name");
// condition is a `kubectl wait` condition such as "jsonpath={.status.ready}=true".
func (c *Cluster) ApplyAndWait(t *testing.T, manifestPath, namespace, resource, condition string, timeout time.Duration) {
	t.Helper()
	c.Kubectl(t, "apply", "-f", manifestPath, "-n", namespace)
	c.Kubectl(t, "wait", "-n", namespace, resource, "--for", condition, "--timeout", timeout.String())
}

// PortForward port-forwards a service port to a local port and returns the local
// base URL (e.g. "http://127.0.0.1:18080"). The forward is torn down by the
// returned stop func and also on test cleanup. service is a kubectl target
// (e.g. "svc/cp-control-plane-api").
func (c *Cluster) PortForward(t *testing.T, namespace, service string, svcPort, localPort int) (baseURL string, stop func()) {
	t.Helper()
	mustBin(t, "kubectl")
	cmd := exec.Command("kubectl", "--kubeconfig", c.Kubeconfig,
		"port-forward", "-n", namespace, service, fmt.Sprintf("%d:%d", localPort, svcPort))
	if err := cmd.Start(); err != nil {
		t.Fatalf("port-forward start: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stopFn := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	}
	t.Cleanup(stopFn)

	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("port-forward exited early")
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return fmt.Sprintf("http://%s", addr), stopFn
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("port-forward to %s/%s:%d never became ready", namespace, service, svcPort)
	return "", stopFn
}

// HTTPClient returns a client with a short timeout suitable for the tests.
func HTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

// mustBin fails the test if the named binary is not on PATH.
func mustBin(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("required binary %q not on PATH: %v", name, err)
	}
}

// run executes a command with a generous timeout and fails the test on error,
// returning combined stdout. The 15m cap is a backstop; long waits are bounded
// by their own flags (helm --timeout 5m, kubectl wait --timeout) so this only
// guards against a truly hung operation.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
