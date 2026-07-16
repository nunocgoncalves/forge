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
				Labels:     map[string]string{"forge.horizonshift.io/env": "test"},
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
	assert.Equal(t, "test", h.Labels["forge.horizonshift.io/env"])
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
	assert.Equal(t, "oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform", c.Spec.Chart.Repository)
	assert.Equal(t, "opo1", c.Spec.Chart.Release)
	assert.Equal(t, "iterabase-system", c.Spec.Chart.Namespace)
}

func TestParse_ChartEmptySkipsDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.Empty(t, c.Spec.Chart.Version)
	assert.Empty(t, c.Spec.Chart.Repository)
}

func TestParse_GPUDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.GPU = GPU{Enabled: true}
	}))
	require.NoError(t, err)
	assert.True(t, c.Spec.GPU.Enabled)
	assert.Equal(t, defaultGPUOperatorVersion, c.Spec.GPU.Operator.Version)
	assert.Equal(t, defaultGPUOperatorRepository, c.Spec.GPU.Operator.Repository)
	assert.Equal(t, defaultGPUOperatorChart, c.Spec.GPU.Operator.Chart)
	assert.Equal(t, "opo1-gpu-operator", c.Spec.GPU.Operator.Release)
	assert.Equal(t, defaultGPUOperatorNamespace, c.Spec.GPU.Operator.Namespace)
}

func TestParse_GPUDisabledNoDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.False(t, c.Spec.GPU.Enabled)
	assert.Empty(t, c.Spec.GPU.Operator.Version)
}

func TestGPUValidate_RequiresSingleNode(t *testing.T) {
	err := GPU{Enabled: true}.validate(ModeHA)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single-node")
}

func TestParse_OverlayDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{Repo: "https://github.com/example/iterabase-overlay.git"}
	}))
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/example/iterabase-overlay.git", c.Spec.Overlay.Repo)
	assert.Equal(t, DefaultOverlayRef, c.Spec.Overlay.Ref, "overlay.ref defaults to master when repo is set")
}

func TestParse_OverlayEmptySkips(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{} // no repo => overlay phase skipped, no default applied
	}))
	require.NoError(t, err)
	assert.Empty(t, c.Spec.Overlay.Repo)
	assert.Empty(t, c.Spec.Overlay.Ref, "no default ref when repo is empty")
}

func TestParse_OverlayBadScheme(t *testing.T) {
	for _, repo := range []string{
		"git@github.com:example/iterabase-overlay.git",
		"ssh://git@github.com/example/iterabase-overlay.git",
		"example/iterabase-overlay",
	} {
		_, err := Parse(yamlFor(t, func(cc *Cluster) {
			cc.Spec.Overlay = Overlay{Repo: repo, Ref: "master"}
		}))
		assert.ErrorContains(t, err, "overlay.repo", "expected scheme error for %q", repo)
	}
}

func TestParse_OverlayFileURL(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{Repo: "file:///tmp/overlay"}
	}))
	require.NoError(t, err)
	assert.Equal(t, "file:///tmp/overlay", c.Spec.Overlay.Repo)
	assert.Equal(t, DefaultOverlayRef, c.Spec.Overlay.Ref)
}

func TestParse_FluxDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{Repo: "https://github.com/example/iterabase-overlay.git"}
		cc.Spec.Flux = Flux{Enabled: true}
	}))
	require.NoError(t, err)
	assert.True(t, c.Spec.Flux.Enabled)
	assert.Equal(t, defaultFluxVersion, c.Spec.Flux.Version, "flux.version defaults when enabled + omitted")
}

func TestParse_FluxDisabledNoDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.False(t, c.Spec.Flux.Enabled)
	assert.Empty(t, c.Spec.Flux.Version, "no default version when Flux disabled")
}

func TestParse_FluxKeepsExplicitVersion(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{Repo: "https://github.com/example/iterabase-overlay.git"}
		cc.Spec.Flux = Flux{Enabled: true, Version: "v9.9.9"}
	}))
	require.NoError(t, err)
	assert.Equal(t, "v9.9.9", c.Spec.Flux.Version, "explicit version preserved")
}

func TestFluxValidate_RequiresOverlay(t *testing.T) {
	err := Flux{Enabled: true, Version: defaultFluxVersion}.validate(Overlay{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlay.repo")
}

func TestFluxValidate_RequiresHTTPS(t *testing.T) {
	err := Flux{Enabled: true, Version: defaultFluxVersion}.validate(Overlay{Repo: "file:///tmp/overlay", Ref: "master"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://")
	assert.Contains(t, err.Error(), "file://")
}

func TestFluxValidate_HTTPSOK(t *testing.T) {
	err := Flux{Enabled: true, Version: defaultFluxVersion}.validate(Overlay{Repo: "https://github.com/example/iterabase-overlay.git", Ref: "master"})
	require.NoError(t, err)
}

func TestFluxValidate_DisabledIgnoresOverlay(t *testing.T) {
	// A disabled Flux is valid regardless of overlay (even file:// / empty).
	require.NoError(t, Flux{}.validate(Overlay{}))
	require.NoError(t, Flux{}.validate(Overlay{Repo: "file:///tmp/overlay", Ref: "master"}))
}

func TestParse_FluxEnabledFileURLRejected(t *testing.T) {
	_, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{Repo: "file:///tmp/overlay", Ref: "master"}
		cc.Spec.Flux = Flux{Enabled: true}
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://")
}

func TestParse_FluxEnabledNoOverlayRejected(t *testing.T) {
	_, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Flux = Flux{Enabled: true}
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlay.repo")
}
