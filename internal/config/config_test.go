package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// validCluster returns a config that passes validation.
func validCluster() *Cluster {
	return &Cluster{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "opo1"},
		Spec: Spec{
			Mode: ModeSingleNode,
			Hosts: []Host{{
				Address:    "10.20.0.10",
				SSHUser:    "forge",
				SSHKeyPath: "~/.ssh/forge_ed25519",
				Role:       RoleControlPlaneWorker,
				Labels:     map[string]string{"node-role.horizonshift.io/gpu": "true"},
				Taints:     []Taint{{Key: "nvidia.com/gpu", Value: "true", Effect: "NoSchedule"}},
			}},
			K3s: K3s{
				Version:       "v1.31.5",
				ClusterCIDR:   "10.42.0.0/16",
				ServiceCIDR:   "10.43.0.0/16",
				DualStack:     true,
				ClusterCIDRv6: "fd42::/48",
				ServiceCIDRv6: "fd43::/112",
				Disable:       []string{"traefik", "servicelb"},
			},
		},
	}
}

// yamlFor marshals a (possibly mutated) valid cluster to YAML for parsing tests.
func yamlFor(t *testing.T, fn func(*Cluster)) []byte {
	t.Helper()
	c := validCluster()
	if fn != nil {
		fn(c)
	}
	out, err := yaml.Marshal(c)
	require.NoError(t, err)
	return out
}

func TestParse_Valid(t *testing.T) {
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.Equal(t, APIVersion, c.APIVersion)
	assert.Equal(t, "opo1", c.Metadata.Name)
	require.Len(t, c.Spec.Hosts, 1)
	h := c.Spec.Hosts[0]
	assert.Equal(t, "10.20.0.10", h.Address)
	assert.Equal(t, "forge", h.SSHUser)
	assert.Equal(t, "true", h.Labels["node-role.horizonshift.io/gpu"])
	require.Len(t, h.Taints, 1)
	assert.Equal(t, "NoSchedule", h.Taints[0].Effect)
	assert.True(t, c.Spec.K3s.DualStack)
	assert.Equal(t, []string{"traefik", "servicelb"}, c.Spec.K3s.Disable)
}

func TestParse_DualStackDisabledNoV6Required(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.K3s.DualStack = false
		cc.Spec.K3s.ClusterCIDRv6 = ""
		cc.Spec.K3s.ServiceCIDRv6 = ""
	}))
	require.NoError(t, err)
	assert.False(t, c.Spec.K3s.DualStack)
}

func TestParse_BadAPIVersion(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.APIVersion = "bad" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiVersion")
}

func TestParse_BadName(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Metadata.Name = "OPO 1" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.name")
}

func TestParse_ReservedMode(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Mode = ModeHA }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}

func TestParse_InvalidMode(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Mode = "bogus" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")
}

func TestParse_NoHosts(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Hosts = nil }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly 1 host")
}

func TestParse_TooManyHosts(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) {
		c.Spec.Hosts = append(c.Spec.Hosts, c.Spec.Hosts[0])
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly 1 host")
}

func TestParse_MissingSSHUser(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Hosts[0].SSHUser = "" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sshUser")
}

func TestParse_BadRole(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Hosts[0].Role = RoleWorker }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role")
}

func TestParse_InvalidTaintEffect(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Hosts[0].Taints[0].Effect = "Maybe" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "effect")
}

func TestParse_InvalidCIDR(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.K3s.ClusterCIDR = "not-a-cidr" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clusterCIDR")
}

func TestParse_DualStackMissingV6(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.K3s.ClusterCIDRv6 = "" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clusterCIDRv6")
}

func TestParse_DualStackV6IsV4(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.K3s.ClusterCIDRv6 = "10.99.0.0/16" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IPv6")
}

func TestLoad_NotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)
}

func TestLoad_FromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "forge.yaml")
	require.NoError(t, os.WriteFile(p, yamlFor(t, nil), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, "opo1", c.Metadata.Name)
}

func TestParse_ChartDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Chart = Chart{Version: "0.1.0"}
	}))
	require.NoError(t, err)
	assert.Equal(t, "0.1.0", c.Spec.Chart.Version)
	assert.Equal(t, "oci://ghcr.io/nunocgoncalves/iterabase-platform", c.Spec.Chart.Repository)
	assert.Equal(t, "opo1", c.Spec.Chart.Release)
	assert.Equal(t, "iterabase-system", c.Spec.Chart.Namespace)
}

func TestParse_ChartEmptySkipsDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.Empty(t, c.Spec.Chart.Version)
	assert.Empty(t, c.Spec.Chart.Repository)
}
