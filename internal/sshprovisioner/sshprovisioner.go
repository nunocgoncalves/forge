// Package sshprovisioner implements provisioner.Provisioner over SSH.
package sshprovisioner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"gopkg.in/yaml.v3"

	"github.com/nunocgoncalves/forge/internal/config"
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
	return r, nil
}

// Install implements provisioner.Provisioner.
func (p *SSHProvisioner) Install(ctx context.Context, version string, serverArgs []string) error {
	cmd := fmt.Sprintf("curl -sfL %s | sudo env INSTALL_K3S_VERSION=%s sh -s - %s",
		k3s.InstallScriptURL, shellQuote(version), joinArgs(serverArgs))
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
	if out, err := p.run(ctx, "sudo cat /etc/rancher/k3s/config.yaml"); err == nil {
		st.ClusterCIDR, st.ServiceCIDR, st.DualStack = parseK3sConfig(out)
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

func parseK3sConfig(out string) (clusterCIDR, serviceCIDR string, dualStack bool) {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(out), &m); err != nil {
		return "", "", false
	}
	if v, ok := m["cluster-cidr"].(string); ok {
		clusterCIDR = v
		dualStack = strings.Contains(v, ",")
	}
	if v, ok := m["service-cidr"].(string); ok {
		serviceCIDR = v
	}
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
