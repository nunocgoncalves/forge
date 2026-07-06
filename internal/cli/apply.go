package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Provision or reconcile the cluster from forge.yaml",
		Long: `apply bootstraps k3s on the target host(s) and converges the cluster to the
state declared in forge.yaml. Safe to re-run: it reconciles from reality and
skips work already done. Immutable field changes are refused (use 'forge
destroy' then 'forge apply'); version changes are routed to 'forge upgrade'.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("apply: not implemented")
		},
	}
	cmd.Flags().Bool("dry-run", false, "preflight and print the plan without mutating")
	cmd.Flags().String("kubeconfig-out", "", "path to write the fetched kubeconfig (default ~/.forge/<install>/kubeconfig.yaml)")
	return cmd
}
