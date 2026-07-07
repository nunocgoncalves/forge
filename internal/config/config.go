// Package config defines the forge.yaml substrate config schema and loader.
package config

import (
	"fmt"
	"net"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Supported values.
const (
	APIVersion = "forge.horizonshift.io/v1alpha1"
	Kind       = "Cluster"

	ModeSingleNode    = "single-node"
	ModeHA            = "ha" // reserved, not yet supported
	ModeBYOKubeconfig = "byokubeconfig"

	RoleControlPlaneWorker = "control-plane+worker"
	RoleControlPlane       = "control-plane"
	RoleWorker             = "worker"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Cluster is the top-level forge.yaml document.
type Cluster struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// Metadata identifies the install.
type Metadata struct {
	Name string `yaml:"name"`
}

// Spec is the cluster substrate specification.
type Spec struct {
	Mode    string  `yaml:"mode"`
	Hosts   []Host  `yaml:"hosts"`
	K3s     K3s     `yaml:"k3s"`
	Flux    Flux    `yaml:"flux"`
	Overlay Overlay `yaml:"overlay"`
	Chart   Chart   `yaml:"chart"`
	GPU     GPU     `yaml:"gpu"`
}

// Host describes a single target VM/host.
type Host struct {
	Address    string            `yaml:"address"`
	SSHUser    string            `yaml:"sshUser"`
	SSHKeyPath string            `yaml:"sshKeyPath"`
	Role       string            `yaml:"role"`
	Labels     map[string]string `yaml:"labels"`
	Taints     []Taint           `yaml:"taints"`
}

// Taint is a Kubernetes node taint applied at install time.
type Taint struct {
	Key    string `yaml:"key"`
	Value  string `yaml:"value"`
	Effect string `yaml:"effect"`
}

// K3s holds k3s bootstrap options.
type K3s struct {
	Version       string   `yaml:"version"`
	ClusterCIDR   string   `yaml:"clusterCIDR"`
	ServiceCIDR   string   `yaml:"serviceCIDR"`
	DualStack     bool     `yaml:"dualStack"`
	ClusterCIDRv6 string   `yaml:"clusterCIDRv6"`
	ServiceCIDRv6 string   `yaml:"serviceCIDRv6"`
	Disable       []string `yaml:"disable"`
	ExtraArgs     []string `yaml:"extraArgs"`
}

// Flux is the (reserved) Flux install toggle.
type Flux struct {
	Enabled bool `yaml:"enabled"`
}

// Overlay is the (reserved) overlay repo pointer.
type Overlay struct {
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
	Path   string `yaml:"path"`
}

// Chart is the platform umbrella chart pull pointer. An empty Version means
// the chart phase is skipped (k3s-only). Defaults are applied in Validate.
type Chart struct {
	Repository string `yaml:"repository"` // OCI URL, e.g. oci://ghcr.io/.../iterabase-platform
	Version    string `yaml:"version"`    // chart version (semver) to install; empty => skip chart
	Release    string `yaml:"release"`    // helm release name (default: metadata.name)
	Namespace  string `yaml:"namespace"`  // target namespace (default: iterabase-system)
}

const (
	defaultChartRepository = "oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform"
	defaultChartNamespace  = "iterabase-system"
)

// applyDefaults fills the chart pull pointer defaults when a chart version is
// set. With no version, the chart phase is skipped and defaults stay empty.
func (c *Chart) applyDefaults(install string) {
	if c.Version == "" {
		return
	}
	if c.Repository == "" {
		c.Repository = defaultChartRepository
	}
	if c.Release == "" {
		c.Release = install
	}
	if c.Namespace == "" {
		c.Namespace = defaultChartNamespace
	}
}

// GPU is the opt-in NVIDIA GPU node-readiness configuration. When Enabled,
// forge installs the NVIDIA GPU Operator (containerized driver + container
// toolkit + device plugin + RuntimeClass via CDI) as a forge-managed Helm
// release and gates apply on the operator's ClusterPolicy reaching ready. It is
// a forge composable dependency, not a chart value — the overlay owns those.
// v1: single-node, Ubuntu hosts only.
type GPU struct {
	Enabled  bool        `yaml:"enabled"`
	Operator GPUOperator `yaml:"operator"`
}

// GPUOperator is the NVIDIA GPU Operator Helm release pointer. Defaults are
// applied in Validate when GPU is enabled; an empty Version falls back to the
// forge-pinned default.
type GPUOperator struct {
	Version    string `yaml:"version"`    // chart version (semver); default defaultGPUOperatorVersion
	Repository string `yaml:"repository"` // helm repo URL; default the NGC chart repo
	Chart      string `yaml:"chart"`      // chart name in the repo; default gpu-operator
	Release    string `yaml:"release"`    // helm release name (default: <metadata.name>-gpu-operator)
	Namespace  string `yaml:"namespace"`  // target namespace (default: gpu-operator)
}

const (
	defaultGPUOperatorVersion    = "v26.3.3"
	defaultGPUOperatorRepository = "https://helm.ngc.nvidia.com/nvidia"
	defaultGPUOperatorChart      = "gpu-operator"
	defaultGPUOperatorNamespace  = "gpu-operator"
)

// applyDefaults fills the GPU operator release pointer defaults when GPU is
// enabled. When disabled, the GPU phase is skipped and defaults stay empty.
func (g *GPU) applyDefaults(install string) {
	if !g.Enabled {
		return
	}
	if g.Operator.Version == "" {
		g.Operator.Version = defaultGPUOperatorVersion
	}
	if g.Operator.Repository == "" {
		g.Operator.Repository = defaultGPUOperatorRepository
	}
	if g.Operator.Chart == "" {
		g.Operator.Chart = defaultGPUOperatorChart
	}
	if g.Operator.Release == "" {
		g.Operator.Release = install + "-gpu-operator"
	}
	if g.Operator.Namespace == "" {
		g.Operator.Namespace = defaultGPUOperatorNamespace
	}
}

// validate enforces v1 constraints on the GPU configuration. GPU readiness
// supports single-node only in v1 (HA is already refused by mode validation;
// this guard makes the intent explicit and keeps GPU enablement honest).
func (g GPU) validate(mode string) error {
	if !g.Enabled {
		return nil
	}
	if mode != ModeSingleNode {
		return fmt.Errorf("gpu.enabled requires mode %q in v1, got %q", ModeSingleNode, mode)
	}
	return nil
}

// Load reads and parses a forge.yaml config from path.
func Load(path string) (*Cluster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return Parse(data)
}

// Parse parses and validates a forge.yaml config from bytes.
func Parse(data []byte) (*Cluster, error) {
	var c Cluster
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the config against v1 constraints.
func (c *Cluster) Validate() error {
	if c.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, c.APIVersion)
	}
	if c.Kind != Kind {
		return fmt.Errorf("kind must be %q, got %q", Kind, c.Kind)
	}
	if c.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if !nameRe.MatchString(c.Metadata.Name) {
		return fmt.Errorf("metadata.name %q must be lowercase alphanumeric with hyphens", c.Metadata.Name)
	}
	c.Spec.Chart.applyDefaults(c.Metadata.Name)
	c.Spec.GPU.applyDefaults(c.Metadata.Name)
	return c.Spec.validate()
}

func (s *Spec) validate() error {
	switch s.Mode {
	case ModeSingleNode:
		if len(s.Hosts) != 1 {
			return fmt.Errorf("mode %q requires exactly 1 host, got %d", s.Mode, len(s.Hosts))
		}
	case ModeHA, ModeBYOKubeconfig:
		return fmt.Errorf("mode %q is reserved and not yet supported", s.Mode)
	default:
		return fmt.Errorf("mode %q is invalid (expected %q)", s.Mode, ModeSingleNode)
	}
	for i, h := range s.Hosts {
		if err := h.validate(); err != nil {
			return fmt.Errorf("host[%d]: %w", i, err)
		}
	}
	if err := s.GPU.validate(s.Mode); err != nil {
		return err
	}
	return s.K3s.validate()
}

func (h *Host) validate() error {
	if h.Address == "" {
		return fmt.Errorf("address is required")
	}
	if h.SSHUser == "" {
		return fmt.Errorf("sshUser is required")
	}
	if h.SSHKeyPath == "" {
		return fmt.Errorf("sshKeyPath is required")
	}
	if h.Role != RoleControlPlaneWorker {
		return fmt.Errorf("role must be %q for v1, got %q", RoleControlPlaneWorker, h.Role)
	}
	for i, t := range h.Taints {
		if err := t.validate(); err != nil {
			return fmt.Errorf("taints[%d]: %w", i, err)
		}
	}
	return nil
}

func (t Taint) validate() error {
	if t.Key == "" {
		return fmt.Errorf("key is required")
	}
	switch t.Effect {
	case "NoSchedule", "PreferNoSchedule", "NoExecute":
	default:
		return fmt.Errorf("effect %q must be NoSchedule|PreferNoSchedule|NoExecute", t.Effect)
	}
	return nil
}

func (k K3s) validate() error {
	if k.Version == "" {
		return fmt.Errorf("k3s.version is required")
	}
	if err := validateCIDR(k.ClusterCIDR, false, "clusterCIDR"); err != nil {
		return err
	}
	if err := validateCIDR(k.ServiceCIDR, false, "serviceCIDR"); err != nil {
		return err
	}
	if k.DualStack {
		if err := validateCIDR(k.ClusterCIDRv6, true, "clusterCIDRv6"); err != nil {
			return err
		}
		if err := validateCIDR(k.ServiceCIDRv6, true, "serviceCIDRv6"); err != nil {
			return err
		}
	}
	return nil
}

func validateCIDR(s string, wantV6 bool, field string) error {
	if s == "" {
		return fmt.Errorf("k3s.%s is required", field)
	}
	_, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return fmt.Errorf("k3s.%s %q: %w", field, s, err)
	}
	isV6 := ipnet.IP.To4() == nil
	if isV6 != wantV6 {
		want := "IPv4"
		if wantV6 {
			want = "IPv6"
		}
		return fmt.Errorf("k3s.%s %q: expected %s", field, s, want)
	}
	return nil
}
