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
	Preflight     *provisioner.PreflightResult
	Installed     bool
	Action        Action
	Reason        string
	ImmutableDiff []string
	HaveVersion   string
	WantVersion   string
}

// Result is the outcome of a mutating apply.
type Result struct {
	Plan           *ReconcilePlan
	KubeconfigPath string
	NodeReady      bool
}

// ApplyOpts configures an apply run.
type ApplyOpts struct {
	KubeconfigOut string
	DryRun        bool
	ReadyTimeout  time.Duration // default 120s
	ReadyInterval time.Duration // default 2s
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

	plan := &ReconcilePlan{Preflight: pf, WantVersion: cfg.Spec.K3s.Version}

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
func Apply(ctx context.Context, cfg *config.Cluster, p provisioner.Provisioner, opts ApplyOpts) (*Result, error) {
	if opts.ReadyTimeout == 0 {
		opts.ReadyTimeout = 120 * time.Second
	}
	if opts.ReadyInterval == 0 {
		opts.ReadyInterval = 2 * time.Second
	}

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

	_ = artifacts.AppendAudit(cfg.Metadata.Name, artifacts.AuditRecord{
		Action: "apply", Result: "success", Version: version.String(),
	})
	return res, nil
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
