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
	res, err := Apply(context.Background(), testConfig(), p, nil, ApplyOpts{
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
	res, err := Apply(context.Background(), testConfig(), p, nil, ApplyOpts{DryRun: true})
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
	_, err := Apply(context.Background(), testConfig(), p, nil, ApplyOpts{})
	require.Error(t, err)
	assert.Empty(t, p.installs)
	assert.Contains(t, err.Error(), "immutable")
}

func TestApply_NodeNotReady(t *testing.T) {
	useTempHome(t)
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), ready: false}
	_, err := Apply(context.Background(), testConfig(), p, nil, ApplyOpts{
		ReadyTimeout: 100 * time.Millisecond, ReadyInterval: 20 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready")
}

func TestApply_KubeconfigOut(t *testing.T) {
	useTempHome(t)
	out := filepath.Join(t.TempDir(), "kc.yaml")
	p := &fakeProv{pf: readyPf(), kubeconfig: []byte(minKubeconfig), readyAfterInstall: true}
	res, err := Apply(context.Background(), testConfig(), p, nil, ApplyOpts{
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
	values                                  []string
}
type repoCall struct{ name, url string }
type uninstallCall struct{ release, namespace string }

// fakeDeployer is a controllable deployer.Deployer for lifecycle chart tests.
type fakeDeployer struct {
	applyCalls     []applyCall
	repoCalls      []repoCall
	uninstallCalls []uninstallCall
	statusState    deployer.ChartState
	applyErr       error
}

func (f *fakeDeployer) Apply(_ context.Context, release, repo, version, ns string, values []string) error {
	f.applyCalls = append(f.applyCalls, applyCall{release, repo, version, ns, values})
	return f.applyErr
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
	res, err := Apply(context.Background(), testConfigWithChart(), p, d, ApplyOpts{
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
	_, err := Apply(context.Background(), testConfigWithChart(), p, d, ApplyOpts{
		SkipChart: true, ReadyTimeout: 1 * time.Second, ReadyInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Empty(t, d.applyCalls)
}

func TestDestroy_Chart(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfigWithChart(), p, d))
	require.Len(t, d.uninstallCalls, 1)
	assert.Equal(t, "opo1", d.uninstallCalls[0].release)
	assert.False(t, p.state.Installed) // k3s uninstalled too
}

func TestDestroy_NoChart(t *testing.T) {
	p := &fakeProv{pf: readyPf(), state: inSyncState()}
	p.pf.Installed = true
	d := &fakeDeployer{}
	require.NoError(t, Destroy(context.Background(), testConfig(), p, d))
	assert.Empty(t, d.uninstallCalls) // no chart configured
	assert.False(t, p.state.Installed)
}
