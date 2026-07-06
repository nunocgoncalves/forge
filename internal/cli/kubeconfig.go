package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newKubeconfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kubeconfig",
		Short: "Fetch or refresh the cluster kubeconfig",
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("kubeconfig: not implemented")
		},
	}
}
