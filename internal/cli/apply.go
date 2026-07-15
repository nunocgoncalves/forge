package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/forge/internal/config"
	"github.com/nunocgoncalves/forge/internal/lifecycle"
	"github.com/nunocgoncalves/forge/internal/sshprovisioner"
)

func newApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Provision or reconcile the cluster from forge.yaml",
		Long: `apply bootstraps k3s on the target host(s) and converges the cluster to the
state declared in forge.yaml. Safe to re-run: it reconciles from reality and
skips work already done. Immutable field changes are refused (use 'forge
destroy' then 'forge apply'); version changes are routed to 'forge upgrade'.`,
		RunE: runApply,
	}
	cmd.Flags().Bool("dry-run", false, "preflight and print the plan without mutating")
	cmd.Flags().Bool("skip-chart", false, "skip the platform chart phase (k3s only)")
	cmd.Flags().Bool("skip-gpu", false, "skip the GPU readiness phase")
	cmd.Flags().Bool("skip-overlay", false, "skip the overlay phase (clone + chart values + CRD instances)")
	cmd.Flags().Bool("skip-secrets", false, "skip the secret-sync phase (materialize declared Secrets)")
	cmd.Flags().String("overlay", "", "overlay repo URL (client fork; https:// or file://; overrides forge.yaml overlay.repo)")
	cmd.Flags().String("kubeconfig-out", "", "path to write the fetched kubeconfig (default ~/.forge/<install>/kubeconfig.yaml)")
	return cmd
}

func runApply(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	// --overlay overrides forge.yaml overlay.repo (flag > config).
	if overlayRepo, _ := cmd.Flags().GetString("overlay"); overlayRepo != "" {
		cfg.Spec.Overlay.Repo = overlayRepo
		if cfg.Spec.Overlay.Ref == "" {
			cfg.Spec.Overlay.Ref = config.DefaultOverlayRef
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
	}
	log := newLogger(cmd)

	p, err := sshprovisioner.New(cfg.Spec.Hosts[0])
	if err != nil {
		return err
	}
	defer p.Close()

	ctx := context.Background()
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	if dryRun {
		plan, err := lifecycle.Plan(ctx, cfg, p)
		if err != nil {
			return err
		}
		printPlan(cmd, plan)
		return nil
	}

	kcOut, _ := cmd.Flags().GetString("kubeconfig-out")
	skipChart, _ := cmd.Flags().GetBool("skip-chart")
	skipGPU, _ := cmd.Flags().GetBool("skip-gpu")
	skipOverlay, _ := cmd.Flags().GetBool("skip-overlay")
	skipSecrets, _ := cmd.Flags().GetBool("skip-secrets")

	// Resolve the overlay git token (only when the overlay phase will run).
	// Public repos + CI proceed tokenless; private repos prompt (TTY) or use
	// FORGE_OVERLAY_TOKEN.
	var overlayToken []byte
	if cfg.Spec.Overlay.Repo != "" && !skipOverlay {
		envTok, _ := os.LookupEnv("FORGE_OVERLAY_TOKEN")
		tok, err := resolveOverlayToken(ctx, cfg.Spec.Overlay.Repo, envTok, isTTY(), termPrompter{}, newGithubScopeChecker())
		if err != nil {
			return err
		}
		overlayToken = tok
	}

	log.Info("applying", "install", cfg.Metadata.Name)
	res, err := lifecycle.Apply(ctx, cfg, p, p, p, lifecycle.ApplyOpts{KubeconfigOut: kcOut, SkipChart: skipChart, SkipGPU: skipGPU, SkipOverlay: skipOverlay, SkipSecrets: skipSecrets, OverlayToken: overlayToken, SecretResolver: cliSecretResolver{interactive: isTTY(), prompter: termSecretPrompter{}, out: os.Stderr}})
	if err != nil {
		return err
	}
	printApplyResult(cmd.OutOrStdout(), cfg, res)
	return nil
}

// printApplyResult writes the apply outcome summary.
func printApplyResult(out io.Writer, cfg *config.Cluster, res *lifecycle.Result) {
	fmt.Fprintln(out, "apply complete")
	fmt.Fprintf(out, "  action:     %s\n", res.Plan.Action)
	fmt.Fprintf(out, "  kubeconfig: %s\n", res.KubeconfigPath)
	fmt.Fprintf(out, "  node ready: %v\n", res.NodeReady)
	if cfg.Spec.Chart.Version != "" {
		fmt.Fprintf(out, "  chart:      %s\n", cfg.Spec.Chart.Version)
		fmt.Fprintf(out, "  chart applied: %v\n", res.ChartApplied)
	}
	if cfg.Spec.GPU.Enabled {
		fmt.Fprintf(out, "  gpu operator: %v\n", res.GPUOperatorApplied)
		fmt.Fprintf(out, "  gpu ready: %v\n", res.GPUReady)
	}
	if cfg.Spec.Overlay.Repo != "" {
		fmt.Fprintf(out, "  overlay:        %s@%s\n", cfg.Spec.Overlay.Repo, cfg.Spec.Overlay.Ref)
		fmt.Fprintf(out, "  overlay applied: %v\n", res.OverlayApplied)
		if res.OverlayCommit != "" {
			fmt.Fprintf(out, "  overlay commit: %s\n", res.OverlayCommit)
		}
	}
	if res.SecretsApplied {
		fmt.Fprintf(out, "  secrets applied: true\n")
	}
}

func printPlan(cmd *cobra.Command, plan *lifecycle.ReconcilePlan) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "plan:")
	fmt.Fprintf(out, "  installed: %v\n", plan.Preflight.Installed)
	fmt.Fprintf(out, "  action:    %s\n", plan.Action)
	if plan.Reason != "" {
		fmt.Fprintf(out, "  reason:    %s\n", plan.Reason)
	}
	if len(plan.ImmutableDiff) > 0 {
		fmt.Fprintf(out, "  drift:     %s\n", plan.ImmutableDiff)
	}
	if plan.HaveVersion != "" {
		fmt.Fprintf(out, "  have:      %s\n", plan.HaveVersion)
	}
	fmt.Fprintf(out, "  want:      %s\n", plan.WantVersion)
	if plan.ChartVersion != "" {
		fmt.Fprintf(out, "  chart:     %s\n", plan.ChartVersion)
	}
	if plan.GPUEnabled {
		fmt.Fprintf(out, "  gpu:       %s (enabled)\n", plan.GPUOperatorVersion)
		fmt.Fprintf(out, "  gpu pci:   %v\n", plan.Preflight.HasNVIDIAGPU)
	}
	if plan.OverlayRepo != "" {
		fmt.Fprintf(out, "  overlay:  %s@%s\n", plan.OverlayRepo, plan.OverlayRef)
	}
}
