package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/nunocgoncalves/forge/internal/config"
)

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a forge.yaml config",
		Long:  "Generate a forge.yaml substrate config, interactively or from flags (--non-interactive).",
		RunE:  runInit,
	}
	cmd.Flags().Bool("non-interactive", false, "generate without prompts using flags")
	cmd.Flags().String("path", "forge.yaml", "output path for the generated config")
	cmd.Flags().String("name", "opo1", "install name")
	cmd.Flags().String("address", "", "target host address")
	cmd.Flags().String("ssh-user", "forge", "SSH user (must have passwordless sudo)")
	cmd.Flags().String("ssh-key", "~/.ssh/forge_ed25519", "SSH key path")
	cmd.Flags().String("k3s-version", "v1.31.5+k3s1", "k3s version (full tag, e.g. v1.31.5+k3s1)")
	cmd.Flags().Bool("dual-stack", true, "enable dual-stack IPv4+IPv6")
	cmd.Flags().Bool("force", false, "overwrite an existing config")
	return cmd
}

func runInit(cmd *cobra.Command, _ []string) error {
	nonInteractive, _ := cmd.Flags().GetBool("non-interactive")
	path, _ := cmd.Flags().GetString("path")
	force, _ := cmd.Flags().GetBool("force")

	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; use --force to overwrite", path)
		}
	}

	name, _ := cmd.Flags().GetString("name")
	address, _ := cmd.Flags().GetString("address")
	sshUser, _ := cmd.Flags().GetString("ssh-user")
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	k3sVersion, _ := cmd.Flags().GetString("k3s-version")
	dualStack, _ := cmd.Flags().GetBool("dual-stack")

	if !nonInteractive {
		in := bufio.NewReader(cmd.InOrStdin())
		name = prompt(in, "Install name", name)
		address = prompt(in, "Target host address", address)
		sshUser = prompt(in, "SSH user", sshUser)
		sshKey = prompt(in, "SSH key path", sshKey)
		k3sVersion = prompt(in, "k3s version", k3sVersion)
	}
	if address == "" {
		return fmt.Errorf("address is required")
	}

	cfg := &config.Cluster{
		APIVersion: config.APIVersion,
		Kind:       config.Kind,
		Metadata:   config.Metadata{Name: name},
		Spec: config.Spec{
			Mode: config.ModeSingleNode,
			Hosts: []config.Host{{
				Address: address, SSHUser: sshUser, SSHKeyPath: sshKey,
				Role:   config.RoleControlPlaneWorker,
				Labels: map[string]string{},
				Taints: []config.Taint{},
			}},
			K3s: config.K3s{
				Version:       k3sVersion,
				ClusterCIDR:   "10.42.0.0/16",
				ServiceCIDR:   "10.43.0.0/16",
				DualStack:     dualStack,
				ClusterCIDRv6: "fd42::/48",
				ServiceCIDRv6: "fd43::/112",
				Disable:       []string{"traefik", "servicelb"},
			},
		},
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
	return nil
}

func prompt(in *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}
