package k3s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/forge/internal/config"
)

func cfg() *config.Cluster {
	return &config.Cluster{
		APIVersion: config.APIVersion,
		Kind:       config.Kind,
		Metadata:   config.Metadata{Name: "opo1"},
		Spec: config.Spec{
			Mode: config.ModeSingleNode,
			Hosts: []config.Host{{
				Address: "10.20.0.10", SSHUser: "forge", SSHKeyPath: "~/.ssh/k",
				Role: config.RoleControlPlaneWorker,
				Labels: map[string]string{
					"node-role.horizonshift.io/gpu": "true",
					"z-last":                        "1",
				},
				Taints: []config.Taint{
					{Key: "nvidia.com/gpu", Value: "true", Effect: "NoSchedule"},
					{Key: "dedicated", Effect: "NoSchedule"},
				},
			}},
			K3s: config.K3s{
				Version:       "v1.31.5",
				ClusterCIDR:   "10.42.0.0/16",
				ServiceCIDR:   "10.43.0.0/16",
				DualStack:     true,
				ClusterCIDRv6: "fd42::/48",
				ServiceCIDRv6: "fd43::/112",
				Disable:       []string{"traefik", "servicelb"},
				ExtraArgs:     []string{"--egress-gateway-mode=true"},
			},
		},
	}
}

func TestServerArgs_DualStack(t *testing.T) {
	args := ServerArgs(cfg())
	assert.Equal(t, "server", args[0])
	assert.Contains(t, args, "--cluster-cidr")
	assert.Contains(t, args, "10.42.0.0/16,fd42::/48")
	assert.Contains(t, args, "--service-cidr")
	assert.Contains(t, args, "10.43.0.0/16,fd43::/112")
	assert.Contains(t, args, "--flannel-backend=vxlan")
}

func TestServerArgs_SingleStack(t *testing.T) {
	c := cfg()
	c.Spec.K3s.DualStack = false
	args := ServerArgs(c)
	assert.Contains(t, args, "10.42.0.0/16")
	assert.NotContains(t, args, "10.42.0.0/16,fd42::/48")
}

func TestServerArgs_Disable(t *testing.T) {
	args := ServerArgs(cfg())
	assert.Contains(t, args, "--disable")
	assert.Contains(t, args, "traefik")
	assert.Contains(t, args, "servicelb")
}

func TestServerArgs_LabelsDeterministic(t *testing.T) {
	a1 := ServerArgs(cfg())
	a2 := ServerArgs(cfg())
	require.Equal(t, a1, a2) // deterministic order
	assert.Contains(t, a1, "--node-label")
	assert.Contains(t, a1, "node-role.horizonshift.io/gpu=true")
	assert.Contains(t, a1, "z-last=1")
	// sorted: node-role... must come before z-last
	iGPU := indexOf(a1, "node-role.horizonshift.io/gpu=true")
	iZ := indexOf(a1, "z-last=1")
	require.Greater(t, iZ, iGPU)
}

func TestServerArgs_Taints(t *testing.T) {
	args := ServerArgs(cfg())
	assert.Contains(t, args, "--node-taint")
	assert.Contains(t, args, "nvidia.com/gpu=true:NoSchedule")
	assert.Contains(t, args, "dedicated:NoSchedule")
}

func TestServerArgs_TLSSAN(t *testing.T) {
	args := ServerArgs(cfg())
	assert.Contains(t, args, "--tls-san")
	assert.Contains(t, args, "10.20.0.10")
}

func TestServerArgs_WriteKubeconfigMode(t *testing.T) {
	args := ServerArgs(cfg())
	assert.Contains(t, args, "--write-kubeconfig-mode")
	assert.Contains(t, args, "0600")
}

func TestServerArgs_ExtraArgs(t *testing.T) {
	args := ServerArgs(cfg())
	assert.Contains(t, args, "--egress-gateway-mode=true")
}

func TestInstallEnv(t *testing.T) {
	env := InstallEnv("v1.31.5")
	assert.Equal(t, "v1.31.5", env["INSTALL_K3S_VERSION"])
}

func TestTaintString(t *testing.T) {
	assert.Equal(t, "k=v:NoSchedule", TaintString(config.Taint{Key: "k", Value: "v", Effect: "NoSchedule"}))
	assert.Equal(t, "k:NoSchedule", TaintString(config.Taint{Key: "k", Effect: "NoSchedule"}))
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}
