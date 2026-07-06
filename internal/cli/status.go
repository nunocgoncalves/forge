package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cluster health and drift against forge.yaml",
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("status: not implemented")
		},
	}
}
