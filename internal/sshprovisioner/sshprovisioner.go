// Package sshprovisioner implements provisioner.Provisioner over SSH.
package sshprovisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/nunocgoncalves/forge/internal/config"
	"github.com/nunocgoncalves/forge/internal/deployer"
	"github.com/nunocgoncalves/forge/internal/k3s"
	"github.com/nunocgoncalves/forge/internal/provisioner"
)

const sshPort = "22"

// dialFunc dials an SSH client. Defaults to a context-aware ssh.Dial; tests
// override it to target an in-process fake server.
type dialFunc func(ctx context.Context, network, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error)

// SSHProvisioner implements provisioner.Provisioner over SSH to a single host.
type SSHProvisioner struct {
	host   config.Host
	cfg    *ssh.ClientConfig
	dial   dialFunc
	client *ssh.Client
}

// Option configures an SSHProvisioner (primarily for tests).
type Option func(*SSHProvisioner)

// WithDial overrides the SSH dial function (for fake-server tests).
func WithDial(d dialFunc) Option {
	return func(p *SSHProvisioner) { p.dial = d }
}

// WithSSHConfig overrides the SSH client config (for fake-server tests).
func WithSSHConfig(c *ssh.ClientConfig) Option {
	return func(p *SSHProvisioner) { p.cfg = c }
}

// New builds an SSHProvisioner for host using key-based auth (key file, or the
// SSH agent as fallback). Encrypted keys must be agent-loaded (no passphrase
// prompt).
func New(host config.Host, opts ...Option) (*SSHProvisioner, error) {
	cfg := &ssh.ClientConfig{
		User:            host.SSHUser,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // TODO: known_hosts pinning
		Timeout:         10 * time.Second,
	}
	p := &SSHProvisioner{host: host, cfg: cfg, dial: defaultDial}
	for _, opt := range opts {
		opt(p)
	}
	// If no auth was injected (e.g. by tests via WithSSHConfig), derive it from
	// the key file or the SSH agent.
	if len(p.cfg.Auth) == 0 {
		if err := setAuth(p.cfg, host.SSHKeyPath); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Close releases the underlying SSH connection.
func (p *SSHProvisioner) Close() error {
	if p.client != nil {
		return p.client.Close()
	}
	return nil
}

func defaultDial(ctx context.Context, network, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	d := net.Dialer{Timeout: cfg.Timeout}
	netConn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	conn, chans, reqs, err := ssh.NewClientConn(netConn, addr, cfg)
	if err != nil {
		netConn.Close()
		return nil, err
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

func setAuth(cfg *ssh.ClientConfig, keyPath string) error {
	path := expandPath(keyPath)
	if path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("ssh key %q: %w", path, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("ssh key %q is too open (mode %o); expected 0600/0644", path, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read ssh key %q: %w", path, err)
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return fmt.Errorf("parse ssh key %q: %w", path, err)
		}
		cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
		return nil
	}
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return errors.New("no sshKeyPath and no SSH_AUTH_SOCK; cannot authenticate")
	}
	conn, err := net.Dial("unix", sock) //nolint:gosec // SSH_AUTH_SOCK is a local unix socket to the user's agent
	if err != nil {
		return fmt.Errorf("connect to ssh agent: %w", err)
	}
	ag := agent.NewClient(conn)
	cfg.Auth = []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)}
	return nil
}

func (p *SSHProvisioner) ensureClient(ctx context.Context) (*ssh.Client, error) {
	if p.client != nil {
		return p.client, nil
	}
	c, err := p.dial(ctx, "tcp", net.JoinHostPort(p.host.Address, sshPort), p.cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", p.host.Address, err)
	}
	p.client = c
	return c, nil
}

func (p *SSHProvisioner) run(ctx context.Context, cmd string) (string, error) {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return "", err
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Start(cmd); err != nil {
		return "", fmt.Errorf("ssh start %q: %w", cmd, err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return stdout.String(), fmt.Errorf("ssh run %q: %w; stderr: %s", cmd, err, stderr.String())
		}
		return stdout.String(), nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	}
}

// Preflight implements provisioner.Provisioner.
func (p *SSHProvisioner) Preflight(ctx context.Context) (*provisioner.PreflightResult, error) {
	r := &provisioner.PreflightResult{}
	if out, err := p.run(ctx, "cat /etc/os-release"); err == nil {
		r.OS = parseOS(out)
	}
	if _, err := p.run(ctx, "sudo -n true"); err == nil {
		r.HasSudo = true
	}
	if _, err := p.run(ctx, "command -v curl"); err == nil {
		r.HasCurl = true
	}
	if _, err := p.run(ctx, "pidof systemd"); err == nil {
		r.HasSystemd = true
	}
	if _, err := p.run(ctx, "command -v k3s"); err == nil {
		r.Installed = true
	}
	if _, err := p.run(ctx, "ip -6 addr show scope global"); err == nil {
		r.HasIPv6 = true
	}
	// GPU preflight checks (read-only; only meaningful when GPU is enabled).
	// NVIDIA GPU on the PCI bus via the vendor ID (0x10de) — no driver or
	// pciutils needed. Kernel-headers presence via the /usr/src dir the driver
	// container mounts.
	if _, err := p.run(ctx, "grep -qi 0x10de /sys/bus/pci/devices/*/vendor"); err == nil {
		r.HasNVIDIAGPU = true
	}
	if _, err := p.run(ctx, "test -d /usr/src/linux-headers-$(uname -r)"); err == nil {
		r.KernelHeadersInstalled = true
	}
	return r, nil
}

// Install implements provisioner.Provisioner.
func (p *SSHProvisioner) Install(ctx context.Context, version string, serverArgs []string) error {
	cmd := fmt.Sprintf("curl -sfL %s | sudo env INSTALL_K3S_VERSION=%s sh -s - %s",
		k3s.InstallScriptURL, shellQuote(k3s.ResolveVersion(version)), joinArgs(serverArgs))
	_, err := p.run(ctx, cmd)
	return err
}

// Upgrade implements provisioner.Provisioner. k3s supports in-place upgrade by
// re-running the install script with a new version.
func (p *SSHProvisioner) Upgrade(ctx context.Context, version string, serverArgs []string) error {
	return p.Install(ctx, version, serverArgs)
}

// Uninstall implements provisioner.Provisioner.
func (p *SSHProvisioner) Uninstall(ctx context.Context) error {
	_, err := p.run(ctx, "sudo /usr/local/bin/k3s-uninstall.sh")
	return err
}

// FetchKubeconfig implements provisioner.Provisioner.
func (p *SSHProvisioner) FetchKubeconfig(ctx context.Context) ([]byte, error) {
	out, err := p.run(ctx, "sudo cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// ReadState implements provisioner.Provisioner.
func (p *SSHProvisioner) ReadState(ctx context.Context) (*provisioner.HostState, error) {
	st := &provisioner.HostState{}
	if _, err := p.run(ctx, "command -v k3s"); err != nil {
		return st, nil // not installed
	}
	st.Installed = true
	if out, err := p.run(ctx, "sudo k3s --version"); err == nil {
		st.Version = parseK3sVersion(out)
	}
	if out, err := p.run(ctx, "sudo cat /etc/systemd/system/k3s.service"); err == nil {
		st.ClusterCIDR, st.ServiceCIDR, st.DualStack = parseSystemdUnit(out)
	}
	return st, nil
}

// NodeReady implements provisioner.Provisioner.
func (p *SSHProvisioner) NodeReady(ctx context.Context) (bool, error) {
	out, err := p.run(ctx, "sudo k3s kubectl get --raw=/readyz")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "ok"), nil
}

// aptLockRetryInterval is the delay between apt retries when the apt/dpkg lock
// is held. A fresh Ubuntu VM commonly has apt locked on first boot by
// cloud-init or unattended-upgrades. Overridden in tests to avoid sleeping.
var aptLockRetryInterval = 15 * time.Second

// EnsureDriverBuildDeps implements provisioner.Provisioner. It installs the
// kernel headers matching the running kernel so the GPU operator's driver
// container can compile the NVIDIA module. Ubuntu/apt in v1; idempotent. It
// retries on apt/dpkg lock contention (cloud-init/unattended-upgrades holding
// the lock on first boot) rather than failing fast.
func (p *SSHProvisioner) EnsureDriverBuildDeps(ctx context.Context) error {
	cmd := "sudo apt-get update && sudo apt-get install -y linux-headers-$(uname -r)"
	for attempt := 0; ; attempt++ {
		out, err := p.run(ctx, cmd)
		if err == nil {
			return nil
		}
		lockHeld := isAptLockHeld(err.Error()) || isAptLockHeld(out)
		if !lockHeld || attempt >= 20 {
			return fmt.Errorf("install kernel headers: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(aptLockRetryInterval):
		}
	}
}

// isAptLockHeld reports whether an apt/dpkg error is lock contention
// (retryable) rather than a real failure.
func isAptLockHeld(msg string) bool {
	return strings.Contains(msg, "Could not get lock") ||
		strings.Contains(msg, "Unable to lock") ||
		strings.Contains(msg, "is held by process")
}

// GPUReady implements provisioner.Provisioner. It reads the GPU operator's
// ClusterPolicy status over the host's k3s kubectl. A missing/not-ready CR
// (including before the operator is installed) returns (false, nil) so the
// readiness poll keeps going rather than aborting.
func (p *SSHProvisioner) GPUReady(ctx context.Context) (bool, error) {
	out, err := p.run(ctx, "sudo k3s kubectl get clusterpolicy -o jsonpath='{.items[0].status.state}'")
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(out) == "ready", nil
}

func parseOS(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return ""
}

func parseK3sVersion(out string) string {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) >= 3 && fields[0] == "k3s" && fields[1] == "version" {
		return fields[2]
	}
	return strings.TrimSpace(out)
}

func parseSystemdUnit(out string) (clusterCIDR, serviceCIDR string, dualStack bool) {
	tokens := strings.Fields(out)
	for i, tok := range tokens {
		switch {
		case tok == "--cluster-cidr" && i+1 < len(tokens):
			clusterCIDR = tokens[i+1]
		case strings.HasPrefix(tok, "--cluster-cidr="):
			clusterCIDR = strings.TrimPrefix(tok, "--cluster-cidr=")
		case tok == "--service-cidr" && i+1 < len(tokens):
			serviceCIDR = tokens[i+1]
		case strings.HasPrefix(tok, "--service-cidr="):
			serviceCIDR = strings.TrimPrefix(tok, "--service-cidr=")
		}
	}
	dualStack = strings.Contains(clusterCIDR, ",")
	return clusterCIDR, serviceCIDR, dualStack
}

func expandPath(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func joinArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

const (
	helmInstallScript = "https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3"
	k3sKubeconfigPath = "/etc/rancher/k3s/k3s.yaml"
)

// helmCmd builds a sudo helm command targeting the k3s kubeconfig on the host.
func helmCmd(args ...string) string {
	return joinArgs(append([]string{"sudo", "helm", "--kubeconfig", k3sKubeconfigPath}, args...))
}

// ensureHelm installs helm on the host if it is not already present.
func (p *SSHProvisioner) ensureHelm(ctx context.Context) error {
	if _, err := p.run(ctx, "command -v helm"); err == nil {
		return nil
	}
	if _, err := p.run(ctx, fmt.Sprintf("curl -fsSL %s | sudo bash", helmInstallScript)); err != nil {
		return fmt.Errorf("install helm: %w", err)
	}
	return nil
}

// Apply implements deployer.Deployer: idempotent helm upgrade --install of a
// Helm release, applying the given --set values (empty for the platform chart,
// whose values come from the overlay), ensuring helm first.
func (p *SSHProvisioner) Apply(ctx context.Context, release, repository, version, namespace string, values []string) error {
	if err := p.ensureHelm(ctx); err != nil {
		return err
	}
	args := []string{"upgrade", "--install", release, repository,
		"--version", version,
		"-n", namespace,
		"--create-namespace",
		"--wait",
		"--timeout", "10m",
	}
	for _, v := range values {
		args = append(args, "--set", v)
	}
	if _, err := p.run(ctx, helmCmd(args...)); err != nil {
		return fmt.Errorf("helm install: %w", err)
	}
	return nil
}

// Status implements deployer.Deployer. A missing release is {Installed: false},
// not an error.
func (p *SSHProvisioner) Status(ctx context.Context, release, namespace string) (*deployer.ChartState, error) {
	if err := p.ensureHelm(ctx); err != nil {
		return nil, err
	}
	out, err := p.run(ctx, helmCmd("status", release, "-n", namespace, "-o", "json"))
	if err != nil {
		return &deployer.ChartState{Installed: false}, nil // release not found
	}
	return parseHelmStatus(out)
}

// UninstallChart implements deployer.Deployer. Best-effort: a missing release
// (or absent helm) is not an error so destroy always proceeds to k3s removal.
func (p *SSHProvisioner) UninstallChart(ctx context.Context, release, namespace string) error {
	if _, err := p.run(ctx, "command -v helm"); err != nil {
		return nil // helm absent => nothing to remove
	}
	_, _ = p.run(ctx, helmCmd("uninstall", release, "-n", namespace)) // best-effort
	return nil
}

// EnsureRepo implements deployer.Deployer: idempotent `helm repo add
// --force-update`. Ensures helm first. Used for repo-based charts (the NVIDIA
// GPU Operator); a no-op concern for OCI charts like the platform chart.
func (p *SSHProvisioner) EnsureRepo(ctx context.Context, name, url string) error {
	if err := p.ensureHelm(ctx); err != nil {
		return err
	}
	if _, err := p.run(ctx, helmCmd("repo", "add", "--force-update", name, url)); err != nil {
		return fmt.Errorf("helm repo add %s: %w", name, err)
	}
	return nil
}

type helmStatusJSON struct {
	Info struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Version string `json:"version"`
		} `json:"metadata"`
	} `json:"chart"`
}

func parseHelmStatus(out string) (*deployer.ChartState, error) {
	var hs helmStatusJSON
	if err := json.Unmarshal([]byte(out), &hs); err != nil {
		return nil, fmt.Errorf("parse helm status: %w", err)
	}
	return &deployer.ChartState{
		Installed: true,
		Status:    hs.Info.Status,
		Version:   hs.Chart.Metadata.Version,
	}, nil
}
