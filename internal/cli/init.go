package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a forge.yaml config",
		Long:  "Generate a forge.yaml substrate config, interactively or from flags.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("init: not implemented")
		},
	}
	cmd.Flags().Bool("non-interactive", false, "generate without prompts using flags")
	cmd.Flags().String("path", "forge.yaml", "output path for the generated config")
	return cmd
}
