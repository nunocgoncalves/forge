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

// Chart is the (reserved) platform umbrella chart version.
type Chart struct {
	Version string `yaml:"version"`
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
