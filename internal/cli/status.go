package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/forge/internal/lifecycle"
	"github.com/nunocgoncalves/forge/internal/sshprovisioner"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cluster health and drift against forge.yaml",
		RunE:  runStatus,
	}
}

func runStatus(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}

	p, err := sshprovisioner.New(cfg.Spec.Hosts[0])
	if err != nil {
		return err
	}
	defer p.Close()

	ctx := context.Background()
	plan, err := lifecycle.Plan(ctx, cfg, p)
	if err != nil {
		return err
	}
	ready, _ := p.NodeReady(ctx)

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "install:    %s\n", cfg.Metadata.Name)
	fmt.Fprintf(out, "installed:  %v\n", plan.Preflight.Installed)
	fmt.Fprintf(out, "action:     %s\n", plan.Action)
	if plan.Reason != "" {
		fmt.Fprintf(out, "reason:     %s\n", plan.Reason)
	}
	if plan.HaveVersion != "" {
		fmt.Fprintf(out, "have:       %s\n", plan.HaveVersion)
	}
	fmt.Fprintf(out, "want:       %s\n", plan.WantVersion)
	fmt.Fprintf(out, "node ready: %v\n", ready)
	return nil
}
