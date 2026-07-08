// Package lifecycle orchestrates the forge apply phases against a
// provisioner.Provisioner and computes the reconcile plan.
package lifecycle

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nunocgoncalves/forge/internal/artifacts"
	"github.com/nunocgoncalves/forge/internal/config"
	"github.com/nunocgoncalves/forge/internal/deployer"
	"github.com/nunocgoncalves/forge/internal/k3s"
	"github.com/nunocgoncalves/forge/internal/kubeconfig"
	"github.com/nunocgoncalves/forge/internal/provisioner"
	"github.com/nunocgoncalves/forge/internal/version"
)

// Action is what apply will do for the host.
type Action int

const (
	ActionInstall         Action = iota // k3s not installed
	ActionSkip                          // installed, in sync
	ActionRefuseUpgrade                 // version changed -> forge upgrade
	ActionRefuseImmutable               // immutable field changed -> destroy + reapply
)

func (a Action) String() string {
	switch a {
	case ActionInstall:
		return "install"
	case ActionSkip:
		return "skip"
	case ActionRefuseUpgrade:
		return "refuse-upgrade"
	case ActionRefuseImmutable:
		return "refuse-immutable"
	default:
		return "unknown"
	}
}

// ReconcilePlan is the read-only reconcile decision (also returned by --dry-run).
type ReconcilePlan struct {
	Preflight          *provisioner.PreflightResult
	Installed          bool
	Action             Action
	Reason             string
	ImmutableDiff      []string
	HaveVersion        string
	WantVersion        string
	ChartVersion       string // platform chart version to apply (empty => skip)
	GPUEnabled         bool   // gpu.enabled; the GPU readiness phase will run
	GPUOperatorVersion string // nvidia/gpu-operator chart version to install (empty => GPU disabled)
}

// Result is the outcome of a mutating apply.
type Result struct {
	Plan               *ReconcilePlan
	KubeconfigPath     string
	NodeReady          bool
	ChartApplied       bool
	GPUOperatorApplied bool // nvidia/gpu-operator release installed/upgraded
	GPUReady           bool // ClusterPolicy reached state=ready (the GPU readiness gate)
}

// ApplyOpts configures an apply run.
type ApplyOpts struct {
	KubeconfigOut    string
	DryRun           bool
	ReadyTimeout     time.Duration // default 120s
	ReadyInterval    time.Duration // default 2s
	SkipChart        bool          // skip the platform chart phase (k3s-only)
	SkipGPU          bool          // skip the GPU readiness phase
	GPUReadyTimeout  time.Duration // default 15m (driver compile is slow on first boot)
	GPUReadyInterval time.Duration // default 5s
}

// withDefaults fills zero-valued timeouts/intervals with their defaults.
func (o ApplyOpts) withDefaults() ApplyOpts {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 120 * time.Second
	}
	if o.ReadyInterval == 0 {
		o.ReadyInterval = 2 * time.Second
	}
	if o.GPUReadyTimeout == 0 {
		o.GPUReadyTimeout = 15 * time.Minute
	}
	if o.GPUReadyInterval == 0 {
		o.GPUReadyInterval = 5 * time.Second
	}
	return o
}

// Plan runs preflight + read-state and returns the reconcile decision. It does
// not mutate the host.
func Plan(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner) (*ReconcilePlan, error) {
	host := cfg.Spec.Hosts[0]
	pf, err := p.Preflight(ctx)
	if err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}
	if !pf.HasSudo {
		return nil, fmt.Errorf("preflight: passwordless sudo required for user %q", host.SSHUser)
	}

	plan := &ReconcilePlan{Preflight: pf, WantVersion: cfg.Spec.K3s.Version, ChartVersion: cfg.Spec.Chart.Version}

	if cfg.Spec.GPU.Enabled {
		if !pf.HasNVIDIAGPU {
			return nil, fmt.Errorf("preflight: gpu.enabled is true but no NVIDIA GPU is present on the PCI bus (PCI passthrough is an OPO1/S11 concern, not forge)")
		}
		if !isUbuntu(pf.OS) {
			return nil, fmt.Errorf("preflight: gpu.enabled requires an Ubuntu host in v1, got %q", pf.OS)
		}
		plan.GPUEnabled = true
		plan.GPUOperatorVersion = cfg.Spec.GPU.Operator.Version
	}

	if !pf.Installed {
		if !pf.HasCurl {
			return nil, fmt.Errorf("preflight: curl is required to install k3s")
		}
		if !pf.HasSystemd {
			return nil, fmt.Errorf("preflight: systemd is required to run k3s")
		}
		if cfg.Spec.K3s.DualStack && !pf.HasIPv6 {
			return nil, fmt.Errorf("preflight: dualStack enabled but host has no IPv6")
		}
		plan.Action = ActionInstall
		plan.Reason = "k3s is not installed"
		return plan, nil
	}

	st, err := p.ReadState(ctx)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	plan.Installed = true
	plan.HaveVersion = st.Version

	if diff := immutableDiff(cfg, st); len(diff) > 0 {
		plan.Action = ActionRefuseImmutable
		plan.ImmutableDiff = diff
		plan.Reason = "immutable field(s) changed: " + strings.Join(diff, ", ")
		return plan, nil
	}
	if versionDrift(st.Version, cfg.Spec.K3s.Version) {
		plan.Action = ActionRefuseUpgrade
		plan.Reason = "k3s version changed; use 'forge upgrade'"
		return plan, nil
	}

	plan.Action = ActionSkip
	plan.Reason = "in sync"
	return plan, nil
}

// Apply runs Plan and, unless DryRun, executes the install/reconcile, fetches
// and stores the kubeconfig, and waits for the node to be Ready.
func Apply(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer, opts ApplyOpts) (*Result, error) {
	opts = opts.withDefaults()

	plan, err := Plan(ctx, cfg, p)
	if err != nil {
		return nil, err
	}
	res := &Result{Plan: plan}
	if opts.DryRun {
		return res, nil
	}

	switch plan.Action {
	case ActionRefuseImmutable:
		return res, fmt.Errorf("%s; run 'forge destroy' then 'forge apply'", plan.Reason)
	case ActionRefuseUpgrade:
		return res, fmt.Errorf("%s", plan.Reason)
	case ActionInstall:
		if err := p.Install(ctx, cfg.Spec.K3s.Version, k3s.ServerArgs(cfg)); err != nil {
			auditFail(cfg, "apply", err)
			return res, err
		}
	case ActionSkip:
		// nothing to install
	}

	outPath, err := storeKubeconfig(ctx, cfg, p, opts.KubeconfigOut)
	if err != nil {
		auditFail(cfg, "apply", err)
		return res, err
	}
	res.KubeconfigPath = outPath

	ready, err := waitForReady(ctx, p, opts.ReadyTimeout, opts.ReadyInterval)
	if err != nil {
		auditFail(cfg, "apply", err)
		return res, err
	}
	res.NodeReady = ready
	if !ready {
		err = fmt.Errorf("node not ready after %s", opts.ReadyTimeout)
		auditFail(cfg, "apply", err)
		return res, err
	}

	if err := applyGPU(ctx, cfg, p, d, opts, res); err != nil {
		return res, err
	}

	if err := applyChart(ctx, cfg, d, opts, res); err != nil {
		return res, err
	}

	_ = artifacts.AppendAudit(cfg.Metadata.Name, artifacts.AuditRecord{
		Action: "apply", Result: "success", Version: version.String(),
	})
	return res, nil
}

// applyChart runs the platform chart phase (helm upgrade --install) when a chart
// version is configured and the phase is not skipped. No-op otherwise.
func applyChart(ctx context.Context, cfg *config.Cluster, d deployer.Deployer, opts ApplyOpts, res *Result) error {
	if d == nil || opts.SkipChart || cfg.Spec.Chart.Version == "" {
		return nil
	}
	ch := cfg.Spec.Chart
	if err := d.Apply(ctx, ch.Release, ch.Repository, ch.Version, ch.Namespace, nil); err != nil {
		auditFail(cfg, "apply", err)
		return fmt.Errorf("chart: %w", err)
	}
	res.ChartApplied = true
	return nil
}

// gpuOperatorRepoName is the local Helm repository name forge registers the
// NVIDIA chart repo under. Not user-facing.
const gpuOperatorRepoName = "nvidia"

// applyGPU runs the GPU node-readiness phase: ensure the host can build the
// driver, install/upgrade the NVIDIA GPU Operator release, then gate on the
// operator's ClusterPolicy reaching ready. No-op when GPU is disabled or
// skipped. Runs after the k3s node is Ready and before the platform chart
// (substrate before app) so the first ModelBackend-driven GPU pod can schedule
// immediately.
func applyGPU(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer, opts ApplyOpts, res *Result) error {
	if !cfg.Spec.GPU.Enabled || opts.SkipGPU {
		return nil
	}
	if err := p.EnsureDriverBuildDeps(ctx); err != nil {
		auditFail(cfg, "apply-gpu", err)
		return fmt.Errorf("gpu build deps: %w", err)
	}
	g := cfg.Spec.GPU.Operator
	if err := d.EnsureRepo(ctx, gpuOperatorRepoName, g.Repository); err != nil {
		auditFail(cfg, "apply-gpu", err)
		return fmt.Errorf("gpu operator repo: %w", err)
	}
	chartRef := gpuOperatorRepoName + "/" + g.Chart
	if err := d.Apply(ctx, g.Release, chartRef, g.Version, g.Namespace, gpuOperatorValues()); err != nil {
		auditFail(cfg, "apply-gpu", err)
		return fmt.Errorf("gpu operator: %w", err)
	}
	res.GPUOperatorApplied = true

	ready, err := waitForGPU(ctx, p, opts)
	if err != nil {
		auditFail(cfg, "apply-gpu", err)
		return err
	}
	res.GPUReady = ready
	if !ready {
		err = fmt.Errorf("gpu not ready after %s (ClusterPolicy did not reach state=ready)", opts.GPUReadyTimeout)
		auditFail(cfg, "apply-gpu", err)
		return err
	}
	return nil
}

// waitForGPU polls the GPU operator's ClusterPolicy readiness until it reports
// ready or the timeout elapses. Mirrors waitForReady; errors from GPUReady are
// tolerated (keep polling) since the CR may not exist yet.
func waitForGPU(ctx context.Context, p provisioner.Provisioner, opts ApplyOpts) (bool, error) {
	deadline := time.Now().Add(opts.GPUReadyTimeout)
	for {
		ready, err := p.GPUReady(ctx)
		if err == nil && ready {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(opts.GPUReadyInterval):
		}
	}
}

// gpuOperatorValues returns the forge-internal, prod-ready Helm --set values for
// the NVIDIA GPU Operator. These mirror the chart's defaults and are set
// explicitly so forge's intent is pinned against chart default changes; advanced
// overrides are a fast-follow. CDI is enabled so workloads request
// nvidia.com/gpu with no runtimeClassName.
//
// k3s containerd: the operator does not auto-detect k3s, so the toolkit must be
// pointed at k3s's containerd config + socket via toolkit.env (the operator
// derives the host mounts from these and rewrites them to its in-container
// paths). Without this the toolkit configures /etc/containerd and signals
// /run/containerd/containerd.sock — neither of which k3s uses — and crashes.
// forge is k3s-only in v1; revisit for HA/BYOK. CDI is enabled, but on k3s the
// nvidia runtime is what injects the GPU: workloads must set
// runtimeClassName: nvidia AND request nvidia.com/gpu (a pod that only requests
// nvidia.com/gpu, with no runtimeClassName, gets no device). nvidia stays
// non-default, so non-GPU pods run on runc. This is the Q7 RuntimeClass
// fallback — record it for HOR-306 (ModelBackend vLLM pod spec).
func gpuOperatorValues() []string {
	return []string{
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
	}
}

func isUbuntu(os string) bool { return strings.HasPrefix(os, "Ubuntu") }

// Destroy removes the platform chart (if configured) and then uninstalls k3s.
// Chart removal is best-effort so destroy always proceeds to substrate removal.
func Destroy(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, d deployer.Deployer) error {
	if d != nil && cfg.Spec.Chart.Version != "" {
		ch := cfg.Spec.Chart
		_ = d.UninstallChart(ctx, ch.Release, ch.Namespace)
	}
	if d != nil && cfg.Spec.GPU.Enabled {
		g := cfg.Spec.GPU.Operator
		_ = d.UninstallChart(ctx, g.Release, g.Namespace)
	}
	return p.Uninstall(ctx)
}

// Upgrade re-runs the k3s install script with a new version (in-place upgrade),
// then refreshes the kubeconfig and waits for the node to be Ready. The host
// must already have k3s installed (use apply first).
func Upgrade(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, to string, opts ApplyOpts) (*Result, error) {
	if opts.ReadyTimeout == 0 {
		opts.ReadyTimeout = 120 * time.Second
	}
	if opts.ReadyInterval == 0 {
		opts.ReadyInterval = 2 * time.Second
	}

	st, err := p.ReadState(ctx)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if !st.Installed {
		return nil, fmt.Errorf("k3s not installed; run 'forge apply' first")
	}
	if to == "" {
		to = cfg.Spec.K3s.Version
	}

	if err := p.Upgrade(ctx, to, k3s.ServerArgs(cfg)); err != nil {
		auditFail(cfg, "upgrade", err)
		return nil, err
	}

	res := &Result{}
	outPath, err := storeKubeconfig(ctx, cfg, p, opts.KubeconfigOut)
	if err != nil {
		auditFail(cfg, "upgrade", err)
		return nil, err
	}
	res.KubeconfigPath = outPath

	ready, err := waitForReady(ctx, p, opts.ReadyTimeout, opts.ReadyInterval)
	if err != nil {
		auditFail(cfg, "upgrade", err)
		return nil, err
	}
	res.NodeReady = ready
	if !ready {
		err = fmt.Errorf("node not ready after %s", opts.ReadyTimeout)
		auditFail(cfg, "upgrade", err)
		return nil, err
	}

	_ = artifacts.AppendAudit(cfg.Metadata.Name, artifacts.AuditRecord{
		Action: "upgrade", Result: "success", Version: version.String(),
	})
	return res, nil
}

// storeKubeconfig fetches the kubeconfig from the host, rewrites the server
// URL for off-host use, and writes it to outPath (or the per-install
// artifacts dir when outPath is empty). Returns the final path.
func storeKubeconfig(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, outPath string) (string, error) {
	raw, err := p.FetchKubeconfig(ctx)
	if err != nil {
		return "", err
	}
	kc, err := kubeconfig.RewriteServer(raw, cfg.Spec.Hosts[0].Address, 6443)
	if err != nil {
		return "", err
	}
	if outPath == "" {
		if err := artifacts.WriteKubeconfig(cfg.Metadata.Name, kc); err != nil {
			return "", err
		}
		return artifacts.KubeconfigPath(cfg.Metadata.Name)
	}
	if err := os.WriteFile(outPath, kc, 0o600); err != nil {
		return "", err
	}
	return outPath, nil
}

func waitForReady(ctx context.Context, p provisioner.Provisioner, timeout, interval time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ready, err := p.NodeReady(ctx)
		if err == nil && ready {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func immutableDiff(cfg *config.Cluster, st *provisioner.HostState) []string {
	var diff []string
	if st.ClusterCIDR != "" && st.ClusterCIDR != k3s.DesiredClusterCIDR(cfg.Spec.K3s) {
		diff = append(diff, "k3s.clusterCIDR")
	}
	if st.ServiceCIDR != "" && st.ServiceCIDR != k3s.DesiredServiceCIDR(cfg.Spec.K3s) {
		diff = append(diff, "k3s.serviceCIDR")
	}
	if st.DualStack != cfg.Spec.K3s.DualStack {
		diff = append(diff, "k3s.dualStack")
	}
	return diff
}

func versionDrift(have, want string) bool {
	if have == "" {
		return false
	}
	return normalizeVersion(have) != normalizeVersion(want)
}

func normalizeVersion(v string) string {
	if i := strings.IndexByte(v, '+'); i >= 0 {
		return v[:i]
	}
	return v
}

func auditFail(cfg *config.Cluster, action string, err error) {
	_ = artifacts.AppendAudit(cfg.Metadata.Name, artifacts.AuditRecord{
		Action: action, Result: "failure", Detail: err.Error(), Version: version.String(),
	})
}
