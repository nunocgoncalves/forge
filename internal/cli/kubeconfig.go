package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/forge/internal/artifacts"
	"github.com/nunocgoncalves/forge/internal/kubeconfig"
	"github.com/nunocgoncalves/forge/internal/sshprovisioner"
)

func newKubeconfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Fetch or refresh the cluster kubeconfig",
		RunE:  runKubeconfig,
	}
	cmd.Flags().String("out", "", "write to path (default: stdout)")
	return cmd
}

func runKubeconfig(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	p, err := sshprovisioner.New(cfg.Spec.Hosts[0])
	if err != nil {
		return err
	}
	defer p.Close()

	raw, err := p.FetchKubeconfig(context.Background())
	if err != nil {
		return err
	}
	kc, err := kubeconfig.RewriteServer(raw, cfg.Spec.Hosts[0].Address, 6443)
	if err != nil {
		return err
	}
	// Always refresh the per-install artifact store.
	if err := artifacts.WriteKubeconfig(cfg.Metadata.Name, kc); err != nil {
		return err
	}

	out, _ := cmd.Flags().GetString("out")
	if out == "" {
		fmt.Fprint(cmd.OutOrStdout(), string(kc))
	} else if err := os.WriteFile(out, kc, 0o600); err != nil {
		return err
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", out)
	}
	return nil
}
