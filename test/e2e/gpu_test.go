// Package e2e contains the composed CPU/GPU cloud scenarios. The no-GPU
// preflight runs on the shared CPU fixture; GPU readiness and real inference
// share the cheapest creatable GPU VM. See GPUVMProvisioner for the Verda seam.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"github.com/nunocgoncalves/forge/test/e2e/internal/runner"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// GPUVMProvisioner creates and destroys an ephemeral GPU VM for the GPU e2e.
// The DigitalOcean implementation iterates the cheapest creatable GPU droplets;
// a future Verda implementation can replace it when DO GPU capacity is
// insufficient. Test-harness only.
type GPUVMProvisioner interface {
	Provision(ctx context.Context, runID, pubKeyStr, privKeyPath string) (*GPUVM, error)
	Destroy(ctx context.Context, id int) error
}

// GPUVM is an ephemeral GPU VM reachable over SSH with the forge sudo user.
type GPUVM struct {
	ID          int
	IP          string
	PrivKeyPath string
}

// ErrNoGPUCapacity signals no GPU instance could be created in any region;
// callers skip-loudly rather than fail so DO scarcity doesn't block PRs.
var ErrNoGPUCapacity = errors.New("no GPU capacity available in any region")

type doGPUVMProvisioner struct{ client *godo.Client }

func (p *doGPUVMProvisioner) Provision(ctx context.Context, runID, pubKeyStr, privKeyPath string) (*GPUVM, error) {
	cands, err := gpuCandidates(ctx, p.client)
	if err != nil {
		return nil, fmt.Errorf("list gpu sizes: %w", err)
	}
	var lastErr error
	for _, c := range cands {
		d, err := createDropletIn(ctx, p.client, runID, pubKeyStr, c.region, c.size)
		if err != nil {
			lastErr = err
			continue // capacity/availability -> try next cheapest candidate
		}
		ip, err := waitForIP(ctx, p.client, d.ID)
		if err != nil {
			_, _ = p.client.Droplets.Delete(ctx, d.ID)
			lastErr = err
			continue
		}
		if err := waitForHostReady(ctx, ip, privKeyPath); err != nil {
			_, _ = p.client.Droplets.Delete(ctx, d.ID)
			lastErr = err
			continue
		}
		return &GPUVM{ID: d.ID, IP: ip, PrivKeyPath: privKeyPath}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no candidates")
	}
	return nil, fmt.Errorf("%w: tried %d (size,region) candidates: %v", ErrNoGPUCapacity, len(cands), lastErr)
}

func (p *doGPUVMProvisioner) Destroy(ctx context.Context, id int) error {
	_, err := p.client.Droplets.Delete(ctx, id)
	return err
}

type gpuCandidate struct {
	size, region string
	price        float64
}

// gpuCandidates returns creatable single-GPU (size, region) pairs, cheapest
// first. A size's `available` flag is not a real-time capacity signal, so we
// use its `regions` list (regions that actually offer it) and discover true
// capacity at creation time by falling through on errors.
func gpuCandidates(ctx context.Context, client *godo.Client) ([]gpuCandidate, error) {
	sizes, _, err := client.Sizes.List(ctx, &godo.ListOptions{PerPage: 200})
	if err != nil {
		return nil, err
	}
	var out []gpuCandidate
	for _, s := range sizes {
		if !strings.Contains(s.Slug, "gpu") {
			continue
		}
		if strings.Contains(s.Slug, "x8") { // skip 8-GPU nodes (very expensive)
			continue
		}
		if s.PriceHourly <= 0 {
			continue
		}
		for _, r := range s.Regions {
			out = append(out, gpuCandidate{size: s.Slug, region: r, price: s.PriceHourly})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].price < out[j].price })
	return out, nil
}

// createDropletIn creates a droplet in an explicit region/size (generalized
// createDroplet for the CPU preflight-fail and GPU provisioner paths).
func createDropletIn(ctx context.Context, client *godo.Client, name, pubKeyStr, region, sizeSlug string) (*godo.Droplet, error) {
	req := &godo.DropletCreateRequest{
		Name:     name,
		Region:   region,
		Size:     sizeSlug,
		UserData: cloudInit(pubKeyStr),
		IPv6:     false,
		Tags:     []string{"forge-e2e", "forge-gpu-e2e", name},
		Image:    godo.DropletCreateImage{Slug: "ubuntu-24-04-x64"},
	}
	d, _, err := client.Droplets.Create(ctx, req)
	return d, err
}

type digitalOceanGPUState struct {
	ctx          context.Context
	provisioner  GPUVMProvisioner
	runID        string
	keep         bool
	privKeyPath  string
	vm           *GPUVM
	forgeBin     string
	forgeHome    string
	chartVersion string
}

func runDigitalOceanGPU(t *testing.T) {
	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		t.Skip("DIGITALOCEAN_TOKEN not set; skipping DigitalOcean GPU scenario")
	}

	ctx := context.Background()
	client := godo.NewFromToken(token)
	pubKey, privKeyPath := generateKey(t)
	state := &digitalOceanGPUState{
		ctx:         ctx,
		provisioner: &doGPUVMProvisioner{client: client},
		runID:       fmt.Sprintf("forge-gpu-%d", time.Now().Unix()),
		keep:        os.Getenv("FORGE_E2E_KEEP") != "",
		privKeyPath: privKeyPath,
		forgeBin:    buildForge(t),
		forgeHome:   t.TempDir(),
	}
	state.chartVersion = platformChartVersion(t, "")

	vm, err := state.provisioner.Provision(ctx, state.runID, pubKey, privKeyPath)
	if errors.Is(err, ErrNoGPUCapacity) {
		t.Skipf("GPU e2e skipped — no GPU capacity (try later or add Verda): %v", err)
	}
	require.NoError(t, err)
	state.vm = vm
	t.Logf("gpu vm ip %s (keep=%v)", vm.IP, state.keep)
	t.Cleanup(func() { state.cleanup(t) })

	runner.RunStages(t, state,
		runner.Stage[*digitalOceanGPUState]{Name: "apply-gpu-substrate", Run: applyGPUSubstrateStage},
		runner.Stage[*digitalOceanGPUState]{Name: "assert-gpu-smoke", Run: assertGPUSmokeStage},
		runner.Stage[*digitalOceanGPUState]{Name: "apply-platform", Run: applyInferencePlatformStage},
		runner.Stage[*digitalOceanGPUState]{Name: "run-real-inference", Run: runInferenceGPUStage},
	)
}

func applyGPUSubstrateStage(t *testing.T, state *digitalOceanGPUState) {
	cfgPath := writeForgeConfigGPU(t, state.runID, state.vm.IP, state.privKeyPath)
	out := applyWithRetry(t, state.forgeBin, state.forgeHome, cfgPath)
	assertApplyMarkers(t, out, "node ready: true", "gpu ready: true")
	t.Logf("apply output:\n%s", out)
}

func assertGPUSmokeStage(t *testing.T, state *digitalOceanGPUState) {
	checkGPUSmoke(t, filepath.Join(state.forgeHome, state.runID, "kubeconfig.yaml"))
}

func (state *digitalOceanGPUState) cleanup(t *testing.T) {
	t.Helper()
	if state.vm == nil {
		return
	}
	if t.Failed() {
		dumpGPUDiagnostics(t, state.vm.IP, state.privKeyPath)
	}
	if state.keep {
		t.Logf("keeping GPU VM %d (run %s) for debugging", state.vm.ID, state.runID)
		return
	}
	if err := state.provisioner.Destroy(state.ctx, state.vm.ID); err != nil {
		t.Logf("destroy GPU VM %d: %v", state.vm.ID, err)
	}
}

// checkGPUSmoke schedules a one-off pod requesting nvidia.com/gpu that runs
// nvidia-smi. Succeeding proves the full path the ModelBackend contract relies
// on: the resource is schedulable AND a container can actually use the GPU
// (CDI/runtime injection, not just advertisement).
func checkGPUSmoke(t *testing.T, kcPath string) {
	t.Helper()
	restCfg, err := clientcmd.BuildConfigFromFlags("", kcPath)
	require.NoError(t, err)
	cs, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	const name = "gpu-smoke"
	nvidiaRC := "nvidia"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			RuntimeClassName: &nvidiaRC,
			RestartPolicy:    corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "smoke",
				Image:   "nvidia/cuda:12.4.1-base-ubuntu22.04",
				Command: []string{"sh", "-c", "nvidia-smi 2>/dev/null || ls /dev/nvidia* 2>/dev/null"},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
					},
				},
			}},
		},
	}
	_, err = cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	defer func() { _ = cs.CoreV1().Pods("default").Delete(context.Background(), name, metav1.DeleteOptions{}) }()

	deadline := time.Now().Add(8 * time.Minute) // image pull; driver is already up (gate passed)
	for time.Now().Before(deadline) {
		p, gerr := cs.CoreV1().Pods("default").Get(context.Background(), name, metav1.GetOptions{})
		if gerr == nil {
			switch p.Status.Phase {
			case corev1.PodSucceeded:
				return
			case corev1.PodFailed:
				t.Fatalf("gpu smoke pod failed: %+v", p.Status.ContainerStatuses)
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("gpu smoke pod did not succeed within timeout")
}

// writeForgeConfigGPU writes a k3s + GPU (no platform chart) forge.yaml.
func writeForgeConfigGPU(t *testing.T, name, ip, keyPath string) string {
	return writeForgeConfigSpec(t, forgeConfigSpec{
		Name: name, Address: ip, SSHKeyPath: keyPath, GPU: true,
	})
}

// sshRun runs a command on the droplet over SSH and returns combined output.
func sshRun(t *testing.T, ip, keyPath, cmd string) (string, error) {
	t.Helper()
	client, err := sshDial(ip, keyPath)
	if err != nil {
		return "", err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// dumpGPUDiagnostics queries the GPU operator state on the droplet when the
// readiness gate fails, so the cause (driver/toolkit/device-plugin/validator)
// is visible in the test log rather than just "gpu not ready after 15m".
func dumpGPUDiagnostics(t *testing.T, ip, keyPath string) {
	t.Helper()
	t.Log("=== GPU diagnostics ===")
	cmds := []string{
		"sudo k3s kubectl get clusterpolicy -o jsonpath='{range .items[*]}{.metadata.name}: state={.status.state}{\"\\n\"}{end}'",
		"sudo k3s kubectl get pods -n gpu-operator -o wide",
		"sudo k3s kubectl logs -n gpu-operator ds/nvidia-container-toolkit-daemonset --tail=100 --all-containers=true",
		"echo '--- k3s containerd config: nvidia/cdi entries? ---'; sudo grep -iE 'nvidia|cdi|runtime' /var/lib/rancher/k3s/agent/etc/containerd/config.toml 2>/dev/null || echo 'no k3s containerd config or no nvidia/cdi entries'",
		"echo '--- /etc/containerd ---'; sudo ls /etc/containerd/ 2>/dev/null || echo 'no /etc/containerd'",
		"sudo k3s kubectl get events -n gpu-operator --sort-by=.lastTimestamp | tail -15",
	}
	for _, c := range cmds {
		out, err := sshRun(t, ip, keyPath, c)
		t.Logf("$ %s\n%s(err=%v)", c, out, err)
	}
}
