package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

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
	cmd.Flags().String("kubeconfig-out", "", "path to write the fetched kubeconfig (default ~/.forge/<install>/kubeconfig.yaml)")
	return cmd
}

func runApply(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
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
	log.Info("applying", "install", cfg.Metadata.Name)
	res, err := lifecycle.Apply(ctx, cfg, p, lifecycle.ApplyOpts{KubeconfigOut: kcOut})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "apply complete")
	fmt.Fprintf(out, "  action:     %s\n", res.Plan.Action)
	fmt.Fprintf(out, "  kubeconfig: %s\n", res.KubeconfigPath)
	fmt.Fprintf(out, "  node ready: %v\n", res.NodeReady)
	return nil
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
}
