package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/forge/internal/lifecycle"
	"github.com/nunocgoncalves/forge/internal/sshprovisioner"
)

func newUpgradeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade k3s to a new version",
		RunE:  runUpgrade,
	}
	cmd.Flags().String("to", "", "target k3s version (default: version in forge.yaml)")
	return cmd
}

func runUpgrade(cmd *cobra.Command, _ []string) error {
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

	to, _ := cmd.Flags().GetString("to")
	log.Info("upgrading", "install", cfg.Metadata.Name, "to", to)
	res, err := lifecycle.Upgrade(context.Background(), cfg, p, to, lifecycle.ApplyOpts{})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "upgrade complete")
	fmt.Fprintf(out, "  kubeconfig: %s\n", res.KubeconfigPath)
	fmt.Fprintf(out, "  node ready: %v\n", res.NodeReady)
	return nil
}
