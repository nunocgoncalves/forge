package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/forge/internal/artifacts"
	"github.com/nunocgoncalves/forge/internal/lifecycle"
	"github.com/nunocgoncalves/forge/internal/sshprovisioner"
)

func newDestroyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Uninstall k3s and remove local artifacts",
		RunE:  runDestroy,
	}
	cmd.Flags().Bool("yes", false, "skip confirmation prompt")
	return cmd
}

func runDestroy(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	host := cfg.Spec.Hosts[0]

	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		if !confirm(cmd, fmt.Sprintf("Uninstall k3s on %s and remove local artifacts?", host.Address)) {
			return fmt.Errorf("aborted")
		}
	}

	p, err := sshprovisioner.New(host)
	if err != nil {
		return err
	}
	defer p.Close()

	ctx := context.Background()
	if err := lifecycle.Destroy(ctx, cfg, p, p, p, p); err != nil {
		return err
	}
	if dir, err := artifacts.Dir(cfg.Metadata.Name); err == nil {
		_ = os.RemoveAll(dir)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "destroyed")
	return nil
}

func confirm(cmd *cobra.Command, msg string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", msg)
	in := bufio.NewReader(cmd.InOrStdin())
	line, _ := in.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
