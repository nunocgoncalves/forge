package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newDestroyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Uninstall k3s and remove local artifacts",
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("destroy: not implemented")
		},
	}
	cmd.Flags().Bool("yes", false, "skip confirmation prompt")
	return cmd
}
