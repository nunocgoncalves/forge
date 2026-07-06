package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newUpgradeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade k3s to a new version",
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("upgrade: not implemented")
		},
	}
	cmd.Flags().String("to", "", "target k3s version (default: version in forge.yaml)")
	return cmd
}
