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
	if cfg.Spec.Chart.Version != "" {
		cs, _ := p.Status(ctx, cfg.Spec.Chart.Release, cfg.Spec.Chart.Namespace)
		if cs != nil && cs.Installed {
			fmt.Fprintf(out, "chart:      %s (%s)\n", cs.Version, cs.Status)
		} else {
			fmt.Fprintf(out, "chart:      not installed (want %s)\n", cfg.Spec.Chart.Version)
		}
	}
	if cfg.Spec.GPU.Enabled {
		g := cfg.Spec.GPU.Operator
		fmt.Fprintln(out, "gpu:")
		fmt.Fprintf(out, "  enabled:       true\n")
		fmt.Fprintf(out, "  pci present:   %v\n", plan.Preflight.HasNVIDIAGPU)
		fmt.Fprintf(out, "  headers:       %s\n", boolLabel(plan.Preflight.KernelHeadersInstalled, "installed", "absent"))
		if cs, _ := p.Status(ctx, g.Release, g.Namespace); cs != nil && cs.Installed {
			fmt.Fprintf(out, "  operator:      %s (%s)\n", g.Version, cs.Status)
		} else {
			fmt.Fprintf(out, "  operator:      not installed (want %s)\n", g.Version)
		}
		ready, _ := p.GPUReady(ctx)
		fmt.Fprintf(out, "  clusterpolicy: %s\n", boolLabel(ready, "ready", "notReady"))
	}
	return nil
}

func boolLabel(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}
