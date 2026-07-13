package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/forge/internal/config"
	"github.com/nunocgoncalves/forge/internal/deployer"
	"github.com/nunocgoncalves/forge/internal/provisioner"
)

const minKubeconfig = "apiVersion: v1\nclusters:\n- name: default\n  cluster:\n    server: https://127.0.0.1:6443\n"

func testConfig() *config.Cluster {
	return &config.Cluster{
		APIVersion: config.APIVersion,
		Kind:       config.Kind,
		Metadata:   config.Metadata{Name: "opo1"},
		Spec: config.Spec{
			Mode: config.ModeSingleNode,
			Hosts: []config.Host{{
				Address: "10.20.0.10", SSHUser: "forge", SSHKeyPath: "/dev/null",
				Role: config.RoleControlPlaneWorker,
			}},
			K3s: config.K3s{
				Version:       "v1.31.5",
				ClusterCIDR:   "10.42.0.0/16",
				ServiceCIDR:   "10.43.0.0/16",
				DualStack:     true,
				ClusterCIDRv6: "fd42::/48",
				ServiceCIDRv6: "fd43::/112",
				Disable:       []string{"traefik", "servicelb"},
			},
		},
	}
}

type installCall struct {
	version string
	args    []string
}

// fakeProv is a controllable provisioner.Provisioner for lifecycle tests.
type fakeProv struct {
	pf                provisioner.PreflightResult
	state             provisioner.HostState
	ready             bool
	readyAfterInstall bool
	kubeconfig        []byte
	installErr        error
	installs          []installCall
	ensureDepsErr     error
	ensureDepsCalls   int
	gpuReady          bool
}

func (f *fakeProv) Preflight(_ context.Context) (*provisioner.PreflightResult, error) {
	return &f.pf, nil
}
func (f *fakeProv) Install(_ context.Context, version string, args []string) error {
	f.installs = append(f.installs, installCall{version, args})
	if f.installErr != nil {
		return f.installErr
	}
	f.state.Installed = true
	if f.readyAfterInstall {
		f.ready = true
	}
	return nil
}
func (f *fakeProv) Upgrade(ctx context.Context, v string, a []string) error {
	return f.Install(ctx, v, a)
}
func (f *fakeProv) Uninstall(_ context.Context) error {
	f.state.Installed = false
	return nil
}
func (f *fakeProv) FetchKubeconfig(_ context.Context) ([]byte, error) { return f.kubeconfig, nil }
func (f *fakeProv) ReadState(_ context.Context) (*provisioner.HostState, error) {
	s := f.state
	return &s, nil
}
func (f *fakeProv) NodeReady(_ context.Context) (bool, error) { return f.ready, nil }
func (f *fakeProv) EnsureDriverBuildDeps(_ context.Context) error {
	f.ensureDepsCalls++
	return f.ensureDepsErr
}
func (f *fakeProv) GPUReady(_ context.Context) (bool, error) { return f.gpuReady, nil }

func readyPf() provisioner.PreflightResult {
	return provisioner.PreflightResult{HasSudo: true, HasCurl: true, HasSystemd: true, HasIPv6: true}
}

func inSyncState() provisioner.HostState {
	return provisioner.HostState{
		Installed:   true,
		Version:     "v1.31.5+k3s1",
		ClusterCIDR: "10.42.0.0/16,fd42::/48",
		ServiceCIDR: "10.43.0.0/16,fd43::/112",
		DualStack:   true,
	}
}

func useTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("FORGE_HOME", t.TempDir())
}

func TestPlan_Install(t *testing.T) {
	p := &fakeProv{pf: readyPf()} // not installed
	plan, err := Plan(context.Background(), testConfig(), p)
	require.NoError(t, err)
	assert.Equal(t, ActionInstall, plan.Action)
	assert.False(t, plan.Preflight.Installed)
}

func TestPlan_Skip(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	plan, err := Plan(context.Background(), testConfig(), p)
	require.NoError(t, err)
	assert.Equal(t, ActionSkip, plan.Action)
	assert.Equal(t, "v1.31.5+k3s1", plan.HaveVersion)
}

func TestPlan_RefuseImmutable(t *testing.T) {
	st := inSyncState()
	st.ClusterCIDR = "10.99.0.0/16,fd42::/48"
	p := &fakeProv{pf: readyPf(), state: st}
	p.pf.Installed = true
	plan, err := Plan(context.Background(), testConfig(), p)
	require.NoError(t, err)
	assert.Equal(t, ActionRefuseImmutable, plan.Action)
	assert.Contains(t, plan.ImmutableDiff, "k3s.clusterCIDR")
}

func TestPlan_RefuseUpgrade(t *testing.T) {
	st := inSyncState()
	st.Version = "v1.30.0+k3s1"
	p := &fakeProv{pf: readyPf(), state: st}
	p.pf.Installed = true
	plan, err := Plan(context.Background(), testConfig(), p)
	require.NoError(t, err)
	assert.Equal(t, ActionRefuseUpgrade, plan.Action)
}

func TestPlan_PreflightNoSudo(t *testing.T) {
	pf := readyPf()
	pf.HasSudo = false
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfig(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sudo")
}

func TestPlan_PreflightNoIPv6DualStack(t *testing.T) {
	pf := readyPf()
	pf.HasIPv6 = false
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfig(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IPv6")
}

func TestApply_Install(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                readyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
	}
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, p.installs, 1)
	assert.Equal(t, "v1.31.5", p.installs[0].version)
	assert.Contains(t, p.installs[0].args, "server")
	assert.True(t, res.NodeReady)

	// kubeconfig written to artifacts with rewritten server
	kc, err := os.ReadFile(filepath.Join(os.Getenv("FORGE_HOME"), "opo1", "kubeconfig.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(kc), "https://10.20.0.10:6443")
}

func TestApply_DryRun(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig)}
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, ActionInstall, res.Plan.Action)
	assert.Empty(t, p.installs) // no install
	_, err = os.Stat(filepath.Join(os.Getenv("FORGE_HOME"), "opo1"))
	assert.True(t, os.IsNotExist(err)) // no artifacts written
}

func TestApply_RefuseImmutable(t *testing.T) {
	useTempHome(t)
	st := inSyncState()
	st.ClusterCIDR = "10.99.0.0/16,fd42::/48"
	p := &fakeProv{pf: readyPf(), state: st, kubeconfig: []byte(minKubeconfig)}
	p.pf.Installed = true
	_, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{})
	require.Error(t, err)
	assert.Empty(t, p.installs)
	assert.Contains(t, err.Error(), "immutable")
}

func TestApply_NodeNotReady(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), ready: false}
	_, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{
		ReadyTimeout: 100 * time.Millisecond, ReadyInterval: 20 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready")
}

func TestApply_KubeconfigOut(t *testing.T) {
	useTempHome(t)
	out := filepath.Join(t.TempDir(), "kc.yaml")
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	res, err := Apply(context.Background(), testConfig(), p, nil, nil, ApplyOpts{
		KubeconfigOut: out, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Equal(t, out, res.KubeconfigPath)
	kc, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Contains(t, string(kc), "https://10.20.0.10:6443")
}

func TestUpgrade(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), state: inSyncState(), kubeconfig: []byte(minKubeconfig), ready: true}
	p.pf.Installed = true
	res, err := Upgrade(context.Background(), testConfig(), p, "v1.32.0+k3s1", ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Len(t, p.installs, 1) // Upgrade delegates to Install
	assert.Equal(t, "v1.32.0+k3s1", p.installs[0].version)
	assert.True(t, res.NodeReady)
}

func TestUpgrade_NotInstalled(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf()} // not installed
	_, err := Upgrade(context.Background(), testConfig(), p, "v1.32.0", ApplyOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not installed")
}

type applyCall struct {
	release, repository, version, namespace string
	values, valueFiles                      []string
}
type repoCall struct{ name, url string }
type uninstallCall struct{ release, namespace string }

// fakeDeployer is a controllable deployer.Deployer for lifecycle chart tests.
type fakeDeployer struct {
	applyCalls           []applyCall
	repoCalls            []repoCall
	uninstallCalls       []uninstallCall
	applyKustomizeCalls  []string
	deleteKustomizeCalls []string
	statusState          deployer.ChartState
	applyErr             error
}

func (f *fakeDeployer) Apply(_ context.Context, opts deployer.ApplyOpts) error {
	f.applyCalls = append(f.applyCalls, applyCall{
		release: opts.Release, repository: opts.Repository,
		version: opts.Version, namespace: opts.Namespace,
		values: opts.Values, valueFiles: opts.ValueFiles,
	})
	return f.applyErr
}

func (f *fakeDeployer) ApplyKustomize(_ context.Context, dir string) error {
	f.applyKustomizeCalls = append(f.applyKustomizeCalls, dir)
	return nil
}

func (f *fakeDeployer) DeleteKustomize(_ context.Context, dir string) error {
	f.deleteKustomizeCalls = append(f.deleteKustomizeCalls, dir)
	return nil
}
func (f *fakeDeployer) EnsureRepo(_ context.Context, name, url string) error {
	f.repoCalls = append(f.repoCalls, repoCall{name, url})
	return nil
}
func (f *fakeDeployer) Status(_ context.Context, _, _ string) (*deployer.ChartState, error) {
	s := f.statusState
	return &s, nil
}
func (f *fakeDeployer) UninstallChart(_ context.Context, release, ns string) error {
	f.uninstallCalls = append(f.uninstallCalls, uninstallCall{release, ns})
	return nil
}

// fakeOverlayer is a controllable overlayer.Overlayer for lifecycle overlay tests.
type fakeOverlayer struct {
	ensureGitErr error
	cloneCommit  string
	cloneErr     error
	cloneCalls   []cloneCall
	removeCalls  []string
}

type cloneCall struct {
	repo, ref, dest string
	hasToken        bool
}

func (f *fakeOverlayer) EnsureGit(_ context.Context) error { return f.ensureGitErr }
func (f *fakeOverlayer) Clone(_ context.Context, repo, ref, dest string, token []byte) (string, error) {
	f.cloneCalls = append(f.cloneCalls, cloneCall{repo, ref, dest, len(token) > 0})
	if f.cloneErr != nil {
		return "", f.cloneErr
	}
	return f.cloneCommit, nil
}
func (f *fakeOverlayer) Remove(_ context.Context, dest string) error {
	f.removeCalls = append(f.removeCalls, dest)
	return nil
}

func testConfigWithChart() *config.Cluster {
	c := testConfig()
	c.Spec.Chart = config.Chart{
		Version:    "0.1.0",
		Repository: "oci://ghcr.io/nunocgoncalves/iterabase-platform",
		Release:    "opo1",
		Namespace:  "iterabase-system",
	}
	return c
}

func TestApply_Chart(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.ChartApplied)
	require.Len(t, d.applyCalls, 1)
	assert.Equal(t, "0.1.0", d.applyCalls[0].version)
	assert.Equal(t, "opo1", d.applyCalls[0].release)
	assert.Equal(t, "iterabase-system", d.applyCalls[0].namespace)
}

func TestApply_SkipChart(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, nil, ApplyOpts{
		SkipChart: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Empty(t, d.applyCalls)
}

func TestDestroy_Chart(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfigWithChart(), p, d, nil))
	require.Len(t, d.uninstallCalls, 1)
	assert.Equal(t, "opo1", d.uninstallCalls[0].release)
	assert.False(t, p.state.Installed) // k3s uninstalled too
}

func TestDestroy_NoChart(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfig(), p, d, nil))
	assert.Empty(t, d.uninstallCalls) // no chart configured
	assert.False(t, p.state.Installed)
}

func testConfigWithGPU() *config.Cluster {
	c := testConfigWithChart()
	c.Spec.GPU = config.GPU{Enabled: true}
	c.Spec.GPU.Operator = config.GPUOperator{
		Version:    "v26.3.3",
		Repository: "https://helm.ngc.nvidia.com/nvidia",
		Chart:      "gpu-operator",
		Release:    "opo1-gpu-operator",
		Namespace:  "gpu-operator",
	}
	return c
}

func gpuReadyPf() provisioner.PreflightResult {
	pf := readyPf()
	pf.OS = "Ubuntu 24.04 LTS"
	pf.HasNVIDIAGPU = true
	pf.KernelHeadersInstalled = true
	return pf
}

func TestPlan_GPUEnabled(t *testing.T) {
	p := &fakeProv{pf: gpuReadyPf()} // not installed
	plan, err := Plan(context.Background(), testConfigWithGPU(), p)
	require.NoError(t, err)
	assert.True(t, plan.GPUEnabled)
	assert.Equal(t, "v26.3.3", plan.GPUOperatorVersion)
	assert.Equal(t, ActionInstall, plan.Action)
}

func TestPlan_GPUEnabledNoGPU(t *testing.T) {
	pf := gpuReadyPf()
	pf.HasNVIDIAGPU = false
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfigWithGPU(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no NVIDIA GPU")
}

func TestPlan_GPUEnabledNonUbuntu(t *testing.T) {
	pf := gpuReadyPf()
	pf.OS = "Debian GNU/Linux 12 (bookworm)"
	p := &fakeProv{pf: pf}
	_, err := Plan(context.Background(), testConfigWithGPU(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ubuntu")
}

func TestApply_GPU(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                gpuReadyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
		gpuReady:          true,
	}
	d := &fakeDeployer{}
	res, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: 1 * time.Second, GPUReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	// GPU operator applied before the platform chart (substrate before app).
	require.Len(t, d.applyCalls, 2)
	op, chart := d.applyCalls[0], d.applyCalls[1]
	assert.Equal(t, "opo1-gpu-operator", op.release)
	assert.Equal(t, "nvidia/gpu-operator", op.repository)
	assert.Equal(t, "v26.3.3", op.version)
	assert.Equal(t, "gpu-operator", op.namespace)
	assert.Equal(t, []string{
		"cdi.enabled=true",
		"driver.enabled=true",
		"toolkit.enabled=true",
		"devicePlugin.enabled=true",
		"gfd.enabled=true",
		"toolkit.env[0].name=CONTAINERD_CONFIG",
		"toolkit.env[0].value=/var/lib/rancher/k3s/agent/etc/containerd/config.toml",
		"toolkit.env[1].name=CONTAINERD_SOCKET",
		"toolkit.env[1].value=/run/k3s/containerd/containerd.sock",
		"toolkit.env[2].name=CONTAINERD_RUNTIME_CLASS",
		"toolkit.env[2].value=nvidia",
	}, op.values)
	assert.Equal(t, "opo1", chart.release)
	assert.Equal(t, "0.1.0", chart.version)
	assert.Equal(t, 1, p.ensureDepsCalls) // build deps ensured once
	require.Len(t, d.repoCalls, 1)
	assert.Equal(t, "nvidia", d.repoCalls[0].name)
	assert.Equal(t, "https://helm.ngc.nvidia.com/nvidia", d.repoCalls[0].url)
	assert.True(t, res.GPUOperatorApplied)
	assert.True(t, res.GPUReady)
	assert.True(t, res.ChartApplied)
}

func TestApply_SkipGPU(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                gpuReadyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
		gpuReady:          true,
	}
	d := &fakeDeployer{}
	_, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, ApplyOpts{
		SkipGPU: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Empty(t, d.repoCalls)          // no GPU operator repo
	assert.Equal(t, 0, p.ensureDepsCalls) // no build deps
	require.Len(t, d.applyCalls, 1)       // platform chart only
	assert.Equal(t, "opo1", d.applyCalls[0].release)
}

func TestApply_GPU_NotReady(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{
		pf:                gpuReadyPf(),
		kubeconfig:        []byte(minKubeconfig),
		readyAfterInstall: true,
		gpuReady:          false, // ClusterPolicy never reaches ready
	}
	d := &fakeDeployer{}
	_, err := Apply(context.Background(), testConfigWithGPU(), p, d, nil, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		GPUReadyTimeout: 100 * time.Millisecond, GPUReadyInterval: 20 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gpu not ready")
}

func TestDestroy_GPU(t *testing.T) {
	p := &fakeProv{pf: gpuReadyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfigWithGPU(), p, d, nil))
	require.Len(t, d.uninstallCalls, 2)                               // chart + gpu operator
	assert.Equal(t, "opo1", d.uninstallCalls[0].release)              // chart first
	assert.Equal(t, "opo1-gpu-operator", d.uninstallCalls[1].release) // then operator
	assert.False(t, p.state.Installed)                                // then k3s
}

func TestApply_Overlay(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "deadbeef"}
	cfg := testConfigWithChart()
	cfg.Spec.Overlay = config.Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"}

	res, err := Apply(context.Background(), cfg, p, d, o, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.OverlayApplied)
	assert.Equal(t, "deadbeef", res.OverlayCommit)

	// overlay cloned with the configured repo/ref + dest; no token (public).
	require.Len(t, o.cloneCalls, 1)
	assert.Equal(t, "https://github.com/example/iterabase-overlay.git", o.cloneCalls[0].repo)
	assert.Equal(t, "master", o.cloneCalls[0].ref)
	assert.Equal(t, "/var/lib/forge/overlay/opo1", o.cloneCalls[0].dest)
	assert.False(t, o.cloneCalls[0].hasToken)

	// chart applied with overlay value files (-f values.yaml -f values.client.yaml).
	require.Len(t, d.applyCalls, 1)
	assert.Equal(t, []string{"/var/lib/forge/overlay/opo1/values.yaml", "/var/lib/forge/overlay/opo1/values.client.yaml"}, d.applyCalls[0].valueFiles)

	// CRD instances applied via kustomize AFTER the chart (ordering: clone -> chart -> crds).
	require.Len(t, d.applyKustomizeCalls, 1)
	assert.Equal(t, "/var/lib/forge/overlay/opo1/crds/client", d.applyKustomizeCalls[0])
}

func TestApply_Overlay_TokenPassthrough(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{cloneCommit: "abc"}
	cfg := testConfigWithChart()
	cfg.Spec.Overlay = config.Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"}

	_, err := Apply(context.Background(), cfg, p, d, o, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		OverlayToken: []byte("ghp_secret"),
	})
	require.NoError(t, err)
	require.Len(t, o.cloneCalls, 1)
	assert.True(t, o.cloneCalls[0].hasToken, "token passed through to Clone")
}

func TestApply_Overlay_SkippedWhenNoRepo(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{}
	cfg := testConfigWithChart() // no overlay

	res, err := Apply(context.Background(), cfg, p, d, o, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.False(t, res.OverlayApplied)
	assert.Empty(t, o.cloneCalls, "no clone when overlay.repo is empty")
	assert.Empty(t, d.applyKustomizeCalls, "no kustomize apply when no overlay")
	require.Len(t, d.applyCalls, 1)
	assert.Empty(t, d.applyCalls[0].valueFiles, "chart applied with no value files when no overlay")
}

func TestApply_Overlay_SkipFlag(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	d := &fakeDeployer{}
	o := &fakeOverlayer{}
	cfg := testConfigWithChart()
	cfg.Spec.Overlay = config.Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"}

	res, err := Apply(context.Background(), cfg, p, d, o, ApplyOpts{
		ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
		SkipOverlay: true,
	})
	require.NoError(t, err)
	assert.False(t, res.OverlayApplied)
	assert.Empty(t, o.cloneCalls, "SkipOverlay skips the clone")
}

func TestDestroy_Overlay(t *testing.T) {
	p := &fakeProv{}
	d := &fakeDeployer{}
	o := &fakeOverlayer{}
	cfg := testConfigWithChart()
	cfg.Spec.Overlay = config.Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"}

	require.NoError(t, Destroy(context.Background(), cfg, p, d, o))
	require.Len(t, o.removeCalls, 1)
	assert.Equal(t, "/var/lib/forge/overlay/opo1", o.removeCalls[0])
}
