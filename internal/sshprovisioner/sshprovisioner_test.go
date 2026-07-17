package sshprovisioner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/forge/internal/config"
	"github.com/nunocgoncalves/forge/internal/deployer"
	"github.com/nunocgoncalves/forge/internal/k3s"
)

// handler returns canned stdout + exit code for a given remote command.
type handler func(cmd string) (string, int)

// startFakeSSH starts an in-process SSH server accepting a single test key.
// It returns the server address, a client config usable to connect, and a
// cleanup func. The handler is invoked for each "exec" request.
func startFakeSSH(t *testing.T, h handler) (string, *ssh.ClientConfig, func()) {
	t.Helper()
	hostPub, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	require.NoError(t, err)
	hostSSHPub, err := ssh.NewPublicKey(hostPub)
	require.NoError(t, err)

	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	require.NoError(t, err)
	authorized, err := ssh.NewPublicKey(clientPub)
	require.NoError(t, err)
	authorizedBytes := authorized.Marshal()

	srvCfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, pk ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(pk.Marshal(), authorizedBytes) {
			return nil, nil
		}
		return nil, errors.New("unknown key")
	}}
	srvCfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				serveConn(c, srvCfg, h)
			}(conn)
		}
	}()

	clientCfg := &ssh.ClientConfig{
		User:            "forge",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: ssh.FixedHostKey(hostSSHPub),
	}
	cleanup := func() {
		_ = ln.Close()
		wg.Wait()
	}
	return ln.Addr().String(), clientCfg, cleanup
}

func serveConn(conn net.Conn, srvCfg *ssh.ServerConfig, h handler) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, srvCfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			_ = ch.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		go serveSession(ch, h)
	}
}

func serveSession(ch ssh.NewChannel, h handler) {
	sch, reqs, err := ch.Accept()
	if err != nil {
		return
	}
	defer sch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		cmd := parseExecPayload(req.Payload)
		out, code := h(cmd)
		_ = req.Reply(true, nil)
		// Commands that read stdin (runStdin) must drain stdin before exit-status
		// so the client's write completes cleanly: "cat > file" (overlay git cred)
		// and "kubectl apply -f -" (secret-sync; the '-' arg is the stdin marker).
		if strings.Contains(cmd, "cat >") || strings.Contains(cmd, "'-'") {
			_, _ = io.Copy(io.Discard, sch)
		}
		if out != "" {
			_, _ = sch.Write([]byte(out))
		}
		exit := make([]byte, 4)
		binary.BigEndian.PutUint32(exit, uint32(code))
		_, _ = sch.SendRequest("exit-status", false, exit)
		_ = sch.Close()
		return
	}
}

func parseExecPayload(p []byte) string {
	if len(p) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(p[:4])
	if int(n) > len(p)-4 {
		return ""
	}
	return string(p[4 : 4+n])
}

// newProvisioner builds an SSHProvisioner wired to the fake server.
func newProvisioner(t *testing.T, addr string, clientCfg *ssh.ClientConfig) *SSHProvisioner {
	t.Helper()
	p, err := New(config.Host{Address: addr, SSHUser: "forge", SSHKeyPath: "/dev/null"},
		WithSSHConfig(clientCfg),
		WithDial(func(_ context.Context, _, _ string, _ *ssh.ClientConfig) (*ssh.Client, error) {
			return ssh.Dial("tcp", addr, clientCfg)
		}),
	)
	require.NoError(t, err)
	return p
}

func TestPreflight(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch cmd {
		case "cat /etc/os-release":
			return "PRETTY_NAME=\"Ubuntu 24.04 LTS\"\nNAME=\"Ubuntu\"\n", 0
		case "sudo -n true":
			return "", 0
		case "command -v curl":
			return "/usr/bin/curl\n", 0
		case "pidof systemd":
			return "1\n", 0
		case "command -v k3s":
			return "/usr/local/bin/k3s\n", 0
		case "ip -6 addr show scope global":
			return "1: lo ...\n", 0
		case "grep -qi 0x10de /sys/bus/pci/devices/*/vendor":
			return "", 0
		case "test -d /usr/src/linux-headers-$(uname -r)":
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()

	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	r, err := p.Preflight(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Ubuntu 24.04 LTS", r.OS)
	assert.True(t, r.HasSudo)
	assert.True(t, r.HasCurl)
	assert.True(t, r.HasSystemd)
	assert.True(t, r.Installed)
	assert.True(t, r.HasIPv6)
	assert.True(t, r.HasNVIDIAGPU)
	assert.True(t, r.KernelHeadersInstalled)
}

func TestInstall_CommandShape(t *testing.T) {
	var got string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		got = cmd
		if strings.HasPrefix(cmd, "curl -sfL "+k3s.InstallScriptURL) {
			return "", 0
		}
		return "", 1
	})
	defer cleanup()

	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	args := []string{"server", "--cluster-cidr", "10.42.0.0/16,fd42::/48", "--disable", "traefik"}
	err := p.Install(context.Background(), "v1.31.5", args)
	require.NoError(t, err)
	assert.Contains(t, got, "INSTALL_K3S_VERSION='v1.31.5+k3s1'")
	assert.Contains(t, got, "sh -s -")
	assert.Contains(t, got, "'server'")
	assert.Contains(t, got, "'--cluster-cidr'")
	assert.Contains(t, got, "'10.42.0.0/16,fd42::/48'")
}

func TestFetchKubeconfig(t *testing.T) {
	want := "apiVersion: v1\nclusters:\n- name: opo1\n"
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		if cmd == "sudo cat /etc/rancher/k3s/k3s.yaml" {
			return want, 0
		}
		return "", 1
	})
	defer cleanup()

	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	got, err := p.FetchKubeconfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}

func TestReadState_Installed(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch cmd {
		case "command -v k3s":
			return "/usr/local/bin/k3s\n", 0
		case "sudo k3s --version":
			return "k3s version v1.31.5+k3s1 (abcdef)\n", 0
		case "sudo cat /etc/systemd/system/k3s.service":
			// Mirrors the exact unit the k3s install script (get.k3s.io) writes:
			// each ExecStart arg on its own backslash-continued line, single-quoted
			// by the script's quote() helper.
			return "[Unit]\nDescription=Lightweight Kubernetes\n[Service]\nExecStart=/usr/local/bin/k3s \\\n    server \\\n\t'--cluster-cidr' \\\n\t'10.42.0.0/16,fd42::/48' \\\n\t'--service-cidr' \\\n\t'10.43.0.0/16,fd43::/112' \\\n\t'--flannel-backend=vxlan' \\\n\t'--tls-san' \\\n\t'10.20.0.10' \\\n\t'--disable' \\\n\t'traefik' \\\n\t'--disable' \\\n\t'servicelb' \\\n\t'--write-kubeconfig-mode' \\\n\t'0600' \\\n[Install]\n", 0
		default:
			return "", 1
		}
	})
	defer cleanup()

	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	st, err := p.ReadState(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.Equal(t, "v1.31.5+k3s1", st.Version)
	assert.Equal(t, "10.42.0.0/16,fd42::/48", st.ClusterCIDR)
	assert.Equal(t, "10.43.0.0/16,fd43::/112", st.ServiceCIDR)
	assert.True(t, st.DualStack)
}

func TestReadState_NotInstalled(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		return "", 1 // every command fails
	})
	defer cleanup()

	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	st, err := p.ReadState(context.Background())
	require.NoError(t, err)
	assert.False(t, st.Installed)
}

func TestParseSystemdUnit(t *testing.T) {
	// realQuotedDualStack mirrors the exact unit the k3s install script
	// (get.k3s.io) writes for a dual-stack install: every ExecStart arg on its
	// own backslash-continued line, single-quoted by the script's quote().
	const realQuotedDualStack = `[Unit]
Description=Lightweight Kubernetes
[Service]
ExecStart=/usr/local/bin/k3s \
    server \
	'--cluster-cidr' \
	'10.42.0.0/16,fd42::/48' \
	'--service-cidr' \
	'10.43.0.0/16,fd43::/112' \
	'--flannel-backend=vxlan' \
	'--disable' \
	'traefik' \
	'--write-kubeconfig-mode' \
	'0600' \
[Install]
`
	const realQuotedSingleStack = `[Unit]
Description=Lightweight Kubernetes
[Service]
ExecStart=/usr/local/bin/k3s \
    server \
	'--cluster-cidr' \
	'10.42.0.0/16' \
	'--service-cidr' \
	'10.43.0.0/16' \
	'--flannel-backend=vxlan' \
[Install]
`

	tests := []struct {
		name            string
		unit            string
		wantClusterCIDR string
		wantServiceCIDR string
		wantDualStack   bool
	}{
		{
			name:            "install-script quoted dual-stack",
			unit:            realQuotedDualStack,
			wantClusterCIDR: "10.42.0.0/16,fd42::/48",
			wantServiceCIDR: "10.43.0.0/16,fd43::/112",
			wantDualStack:   true,
		},
		{
			name:            "install-script quoted single-stack",
			unit:            realQuotedSingleStack,
			wantClusterCIDR: "10.42.0.0/16",
			wantServiceCIDR: "10.43.0.0/16",
			wantDualStack:   false,
		},
		{
			name:            "unquoted space-separated (legacy)",
			unit:            "ExecStart=/usr/local/bin/k3s server --cluster-cidr 10.42.0.0/16,fd42::/48 --service-cidr 10.43.0.0/16,fd43::/112\n",
			wantClusterCIDR: "10.42.0.0/16,fd42::/48",
			wantServiceCIDR: "10.43.0.0/16,fd43::/112",
			wantDualStack:   true,
		},
		{
			name:            "equals form unquoted",
			unit:            "ExecStart=/usr/local/bin/k3s server --cluster-cidr=10.42.0.0/16,fd42::/48 --service-cidr=10.43.0.0/16,fd43::/112\n",
			wantClusterCIDR: "10.42.0.0/16,fd42::/48",
			wantServiceCIDR: "10.43.0.0/16,fd43::/112",
			wantDualStack:   true,
		},
		{
			name:            "equals form quoted",
			unit:            "ExecStart=/usr/local/bin/k3s \\\n    server \\\n\t'--cluster-cidr=10.42.0.0/16,fd42::/48' \\\n\t'--service-cidr=10.43.0.0/16,fd43::/112' \\\n",
			wantClusterCIDR: "10.42.0.0/16,fd42::/48",
			wantServiceCIDR: "10.43.0.0/16,fd43::/112",
			wantDualStack:   true,
		},
		{
			name: "embedded single quote in value is unescaped",
			// k3s quote() escapes an embedded single quote with the POSIX
			// backslash-quote sequence. The parser must restore the original. Use a
			// raw string so the backslash is preserved literally (in a double-quoted
			// literal a backslash before a quote would collapse to a bare quote).
			unit: `ExecStart=/usr/local/bin/k3s server '--cluster-cidr' 'a'\''b,c::/48'
`,
			wantClusterCIDR: "a'b,c::/48",
			wantServiceCIDR: "",
			wantDualStack:   true,
		},
		{
			name:            "no execstart args",
			unit:            "[Service]\nExecStart=/usr/local/bin/k3s server\n",
			wantClusterCIDR: "",
			wantServiceCIDR: "",
			wantDualStack:   false,
		},
		{
			name:            "empty unit",
			unit:            "",
			wantClusterCIDR: "",
			wantServiceCIDR: "",
			wantDualStack:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc, sc, ds := parseSystemdUnit(tt.unit)
			assert.Equal(t, tt.wantClusterCIDR, cc, "clusterCIDR")
			assert.Equal(t, tt.wantServiceCIDR, sc, "serviceCIDR")
			assert.Equal(t, tt.wantDualStack, ds, "dualStack")
		})
	}
}

func TestNodeReady(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
			if cmd == "sudo k3s kubectl get --raw=/readyz" {
				return "ok", 0
			}
			return "", 1
		})
		defer cleanup()
		p := newProvisioner(t, addr, cfg)
		defer p.Close()
		ok, err := p.NodeReady(context.Background())
		require.NoError(t, err)
		assert.True(t, ok)
	})
	t.Run("not ready", func(t *testing.T) {
		addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
			if cmd == "sudo k3s kubectl get --raw=/readyz" {
				return "etcd: false\n", 0
			}
			return "", 1
		})
		defer cleanup()
		p := newProvisioner(t, addr, cfg)
		defer p.Close()
		ok, err := p.NodeReady(context.Background())
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

func TestUninstall(t *testing.T) {
	var got string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		got = cmd
		if cmd == "sudo /usr/local/bin/k3s-uninstall.sh" {
			return "", 0
		}
		return "", 1
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.Uninstall(context.Background()))
	assert.Equal(t, "sudo /usr/local/bin/k3s-uninstall.sh", got)
}

func TestDeployer_Apply(t *testing.T) {
	var got string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v helm":
			return "/usr/local/bin/helm\n", 0
		case strings.Contains(cmd, "upgrade"):
			got = cmd
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.Apply(context.Background(), deployer.ApplyOpts{
		Release: "opo1", Repository: "oci://ghcr.io/nunocgoncalves/iterabase-platform",
		Version: "0.1.0", Namespace: "iterabase-system",
		Values: []string{"driver.enabled=true", "toolkit.enabled=true"},
	}))
	assert.Contains(t, got, "--set")
	assert.Contains(t, got, "driver.enabled=true")
	assert.Contains(t, got, "toolkit.enabled=true")
}

func TestDeployer_Apply_EnsuresHelm(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v helm":
			return "", 1 // absent
		case strings.Contains(cmd, "get-helm-4"):
			return "", 0 // install script
		case strings.Contains(cmd, "upgrade"):
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.Apply(context.Background(), deployer.ApplyOpts{
		Release: "opo1", Repository: "oci://ghcr.io/nunocgoncalves/iterabase-platform",
		Version: "0.1.0", Namespace: "iterabase-system",
	}))
}

func TestDeployer_Status(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v helm":
			return "/usr/local/bin/helm\n", 0
		case strings.Contains(cmd, "status"):
			return `{"info":{"status":"deployed"},"chart":{"metadata":{"version":"0.1.0"}}}`, 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	st, err := p.Status(context.Background(), "opo1", "iterabase-system")
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.Equal(t, "deployed", st.Status)
	assert.Equal(t, "0.1.0", st.Version)
}

func TestDeployer_Status_NotInstalled(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v helm":
			return "/usr/local/bin/helm\n", 0
		case strings.Contains(cmd, "status"):
			return "", 1 // release not found
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	st, err := p.Status(context.Background(), "opo1", "iterabase-system")
	require.NoError(t, err)
	assert.False(t, st.Installed)
}

func TestDeployer_UninstallChart(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v helm":
			return "/usr/local/bin/helm\n", 0
		case strings.Contains(cmd, "uninstall"):
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.UninstallChart(context.Background(), "opo1", "iterabase-system"))
}

func TestDeployer_UninstallChart_HelmAbsent(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		if cmd == "command -v helm" {
			return "", 1 // absent
		}
		return "", 1
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.UninstallChart(context.Background(), "opo1", "iterabase-system"))
}

func TestPreflight_NoGPU(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch cmd {
		case "cat /etc/os-release":
			return "PRETTY_NAME=\"Ubuntu 24.04 LTS\"\n", 0
		case "sudo -n true", "command -v curl", "pidof systemd", "command -v k3s",
			"ip -6 addr show scope global", "test -d /usr/src/linux-headers-$(uname -r)":
			return "", 0
		case "grep -qi 0x10de /sys/bus/pci/devices/*/vendor":
			return "", 1 // no NVIDIA device
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	r, err := p.Preflight(context.Background())
	require.NoError(t, err)
	assert.False(t, r.HasNVIDIAGPU)
	assert.True(t, r.KernelHeadersInstalled)
}

func TestPreflight_NoKernelHeaders(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch cmd {
		case "cat /etc/os-release":
			return "PRETTY_NAME=\"Ubuntu 24.04 LTS\"\n", 0
		case "sudo -n true", "command -v curl", "pidof systemd", "command -v k3s",
			"ip -6 addr show scope global", "grep -qi 0x10de /sys/bus/pci/devices/*/vendor":
			return "", 0
		case "test -d /usr/src/linux-headers-$(uname -r)":
			return "", 1 // headers absent
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	r, err := p.Preflight(context.Background())
	require.NoError(t, err)
	assert.True(t, r.HasNVIDIAGPU)
	assert.False(t, r.KernelHeadersInstalled)
}

func TestEnsureDriverBuildDeps_CommandShape(t *testing.T) {
	var got string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		got = cmd
		if strings.Contains(cmd, "apt-get install -y linux-headers-$(uname -r)") {
			return "", 0
		}
		return "", 1
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.EnsureDriverBuildDeps(context.Background()))
	assert.Contains(t, got, "apt-get update")
	assert.Contains(t, got, "apt-get install -y linux-headers-$(uname -r)")
}

func TestGPUReady(t *testing.T) {
	const q = "sudo k3s kubectl get clusterpolicy -o jsonpath='{.items[0].status.state}'"
	t.Run("ready", func(t *testing.T) {
		addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
			if cmd == q {
				return "ready", 0
			}
			return "", 1
		})
		defer cleanup()
		p := newProvisioner(t, addr, cfg)
		defer p.Close()
		ok, err := p.GPUReady(context.Background())
		require.NoError(t, err)
		assert.True(t, ok)
	})
	t.Run("not ready", func(t *testing.T) {
		addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
			if cmd == q {
				return "notReady", 0
			}
			return "", 1
		})
		defer cleanup()
		p := newProvisioner(t, addr, cfg)
		defer p.Close()
		ok, err := p.GPUReady(context.Background())
		require.NoError(t, err)
		assert.False(t, ok)
	})
	t.Run("clusterpolicy absent", func(t *testing.T) {
		// Before the operator is installed the CR doesn't exist: kubectl errors,
		// GPUReady returns (false, nil) so the readiness poll keeps going.
		addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
			return "", 1
		})
		defer cleanup()
		p := newProvisioner(t, addr, cfg)
		defer p.Close()
		ok, err := p.GPUReady(context.Background())
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

func TestEnsureRepo_CommandShape(t *testing.T) {
	var got string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v helm":
			return "/usr/local/bin/helm\n", 0
		case strings.Contains(cmd, "--force-update"):
			got = cmd
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.EnsureRepo(context.Background(), "nvidia", "https://helm.ngc.nvidia.com/nvidia"))
	assert.Contains(t, got, "repo")
	assert.Contains(t, got, "add")
	assert.Contains(t, got, "--force-update")
	assert.Contains(t, got, "nvidia")
	assert.Contains(t, got, "https://helm.ngc.nvidia.com/nvidia")
}

func TestEnsureDriverBuildDeps_RetriesOnAptLock(t *testing.T) {
	prev := aptLockRetryInterval
	aptLockRetryInterval = time.Millisecond
	defer func() { aptLockRetryInterval = prev }()

	calls := 0
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		if !strings.Contains(cmd, "apt-get install -y linux-headers-$(uname -r)") {
			return "", 1
		}
		calls++
		if calls < 3 {
			// apt lock held by cloud-init/unattended-upgrades on first boot
			return "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 1238 (apt-get)\n", 100
		}
		return "", 0
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.EnsureDriverBuildDeps(context.Background()))
	assert.Equal(t, 3, calls)
}

func TestEnsureDriverBuildDeps_AptLockHeldTooLong(t *testing.T) {
	prev := aptLockRetryInterval
	aptLockRetryInterval = time.Millisecond
	defer func() { aptLockRetryInterval = prev }()

	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		if strings.Contains(cmd, "apt-get install -y linux-headers-$(uname -r)") {
			return "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 1238 (apt-get)\n", 100
		}
		return "", 1
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	err := p.EnsureDriverBuildDeps(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install kernel headers")
}

func TestIsAptLockHeld(t *testing.T) {
	assert.True(t, isAptLockHeld("ssh run ...; stderr: E: Could not get lock /var/lib/apt/lists/lock"))
	assert.True(t, isAptLockHeld("E: Unable to lock directory /var/lib/apt/lists/"))
	assert.True(t, isAptLockHeld("...is held by process 1238 (apt-get)"))
	assert.False(t, isAptLockHeld("E: Unable to locate package linux-headers-6.8.0"))
}

func TestOverlayer_Clone(t *testing.T) {
	var gotClone, gotCheck string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v git":
			return "/usr/bin/git\n", 0
		case strings.Contains(cmd, "clone"):
			gotClone = cmd
			return "", 0
		case strings.HasPrefix(cmd, "test -f "):
			gotCheck = cmd
			return "", 0
		case strings.Contains(cmd, "rev-parse HEAD"):
			return "deadbeef\n", 0
		default: // rm -rf, mkdir -p
			return "", 0
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	commit, err := p.Clone(context.Background(), "https://github.com/example/overlay.git", "master", "/var/lib/forge/overlay/opo1", nil)
	require.NoError(t, err)
	assert.Equal(t, "deadbeef", commit)
	assert.Contains(t, gotClone, "https://github.com/example/overlay.git")
	assert.Contains(t, gotClone, "/var/lib/forge/overlay/opo1")
	assert.Contains(t, gotCheck, "values.yaml")
	assert.Contains(t, gotCheck, "values.client.yaml")
	assert.Contains(t, gotCheck, "crds/client/kustomization.yaml")
}

func TestOverlayer_Clone_StructureValidation(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v git":
			return "/usr/bin/git\n", 0
		case strings.Contains(cmd, "clone"):
			return "", 0
		case strings.HasPrefix(cmd, "test -f "):
			return "", 1 // structure missing
		default:
			return "", 0
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	_, err := p.Clone(context.Background(), "https://github.com/example/overlay.git", "master", "/var/lib/forge/overlay/opo1", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlay structure")
}

func TestOverlayer_Clone_TokenCredFile(t *testing.T) {
	var gotClone string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v git":
			return "/usr/bin/git\n", 0
		case strings.Contains(cmd, "clone"):
			gotClone = cmd
			return "", 0
		case strings.HasPrefix(cmd, "test -f "):
			return "", 0
		case strings.Contains(cmd, "rev-parse HEAD"):
			return "abc\n", 0
		default: // cat > credFile (runStdin), rm -rf, mkdir -p
			return "", 0
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	_, err := p.Clone(context.Background(), "https://github.com/example/overlay.git", "master", "/var/lib/forge/overlay/opo1", []byte("ghp_secret"))
	require.NoError(t, err)
	assert.NotContains(t, gotClone, "ghp_secret", "token must not appear in the clone command/ps")
	assert.Contains(t, gotClone, "credential.helper=store --file=")
	assert.Contains(t, gotClone, "https://github.com/example/overlay.git", "clone URL has no embedded token")
}

func TestDeployer_ApplyKustomize_Empty(t *testing.T) {
	var applied bool
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case strings.Contains(cmd, "kustomize"):
			return "", 0 // empty render (no objects)
		case strings.Contains(cmd, "apply"):
			applied = true
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.ApplyKustomize(context.Background(), "/var/lib/forge/overlay/opo1/crds/client"))
	assert.False(t, applied, "empty kustomize => apply skipped (no objects)")
}

func TestDeployer_ApplyKustomize_Objects(t *testing.T) {
	var applied bool
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case strings.Contains(cmd, "kustomize"):
			return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n", 0
		case strings.Contains(cmd, "apply"):
			applied = true
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.ApplyKustomize(context.Background(), "/var/lib/forge/overlay/opo1/crds/client"))
	assert.True(t, applied, "non-empty kustomize => apply runs")
}

func TestDeployer_ApplyManifest(t *testing.T) {
	var got string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		if strings.Contains(cmd, "apply") && strings.Contains(cmd, "-f") {
			got = cmd
			return "", 0
		}
		return "", 1
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	// A Secret manifest carrying the value in stringData; it is piped via stdin.
	manifest := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"tok","namespace":"ns"},"stringData":{"api-token":"supersecret"}}`
	require.NoError(t, p.ApplyManifest(context.Background(), manifest))
	assert.Contains(t, got, "kubectl")
	assert.Contains(t, got, "apply")
	assert.Contains(t, got, "-f")
	assert.NotContains(t, got, "supersecret", "value must be piped via stdin, not in the command/ps")
	assert.NotContains(t, got, "stringData", "manifest must be piped via stdin, not in the command")
}

func TestDeployer_ApplyManifest_Error(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		if strings.Contains(cmd, "apply") && strings.Contains(cmd, "-f") {
			return "error: no kind \"Secret\" is registered", 1
		}
		return "", 1
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	err := p.ApplyManifest(context.Background(), `{"apiVersion":"v1","kind":"Secret"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubectl apply -f -")
}

func TestOverlayer_ReadFile(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
			if strings.HasPrefix(cmd, "cat ") {
				return "secrets: []\n", 0
			}
			return "", 1
		})
		defer cleanup()
		p := newProvisioner(t, addr, cfg)
		defer p.Close()
		out, err := p.ReadFile(context.Background(), "/var/lib/forge/overlay/opo1", "secrets.yaml")
		require.NoError(t, err)
		assert.Equal(t, "secrets: []\n", out)
	})
	t.Run("missing", func(t *testing.T) {
		// A missing file makes cat exit non-zero; the real host's stderr carries
		// "No such file" (asserted via the lifecycle fake). Here we only assert
		// the error surfaces — the fake SSH server can't emit stderr.
		addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
			if strings.HasPrefix(cmd, "cat ") {
				return "", 1
			}
			return "", 1
		})
		defer cleanup()
		p := newProvisioner(t, addr, cfg)
		defer p.Close()
		_, err := p.ReadFile(context.Background(), "/var/lib/forge/overlay/opo1", "secrets.yaml")
		require.Error(t, err)
	})
}

func TestFluxer_EnsureFlux_InstallsCLI(t *testing.T) {
	var gotInstall, gotFluxInstall string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v flux":
			return "", 1 // absent
		case strings.Contains(cmd, "fluxcd.io/install.sh"):
			gotInstall = cmd
			return "", 0
		case strings.Contains(cmd, "flux") && strings.Contains(cmd, "install") && strings.Contains(cmd, "--version="):
			gotFluxInstall = cmd
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.EnsureFlux(context.Background(), "v2.4.0"))
	// CLI install script is version-pinned via FLUX_VERSION; the version never
	// appears bare in a way that could mismatch a tag filter.
	assert.Contains(t, gotInstall, "fluxcd.io/install.sh")
	assert.Contains(t, gotInstall, "FLUX_VERSION='2.4.0'", "install script takes the version without the leading v (it prepends v internally)")
	// flux install runs against the k3s kubeconfig via KUBECONFIG env (sudo root
	// reads the root-owned 0600 kubeconfig); version pinned.
	assert.Contains(t, gotFluxInstall, "KUBECONFIG=/etc/rancher/k3s/k3s.yaml")
	assert.Contains(t, gotFluxInstall, "--version=v2.4.0")
}

func TestFluxer_EnsureFlux_CLIPresent(t *testing.T) {
	var gotFluxInstall string
	sawInstallScript := false
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v flux":
			return "/usr/local/bin/flux\n", 0 // present
		case strings.Contains(cmd, "fluxcd.io/install.sh"):
			sawInstallScript = true
			return "", 0
		case strings.Contains(cmd, "flux") && strings.Contains(cmd, "install") && strings.Contains(cmd, "--version="):
			gotFluxInstall = cmd
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.EnsureFlux(context.Background(), "v2.4.0"))
	assert.False(t, sawInstallScript, "CLI already present => install script skipped")
	assert.Contains(t, gotFluxInstall, "--version=v2.4.0")
}

func TestFluxer_EnsureFlux_InstallFails(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v flux":
			return "", 1
		case strings.Contains(cmd, "fluxcd.io/install.sh"):
			return "", 0
		case strings.Contains(cmd, "flux") && strings.Contains(cmd, "install"):
			return "", 1 // flux install fails
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	err := p.EnsureFlux(context.Background(), "v2.4.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flux install")
}

func TestFluxer_UninstallFlux(t *testing.T) {
	var got string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case cmd == "command -v flux":
			return "/usr/local/bin/flux\n", 0
		case strings.Contains(cmd, "flux") && strings.Contains(cmd, "uninstall"):
			got = cmd
			return "", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	require.NoError(t, p.UninstallFlux(context.Background()))
	assert.Contains(t, got, "uninstall")
	assert.Contains(t, got, "--silent") // non-interactive
	assert.Contains(t, got, "KUBECONFIG=/etc/rancher/k3s/k3s.yaml")
}

func TestFluxer_UninstallFlux_FluxAbsent(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		if cmd == "command -v flux" {
			return "", 1 // CLI absent
		}
		return "", 1
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	// No flux CLI => nothing to remove, not an error (destroy proceeds to k3s).
	require.NoError(t, p.UninstallFlux(context.Background()))
}

func TestFluxer_GitRepositoryStatus(t *testing.T) {
	var got string
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		switch {
		case strings.Contains(cmd, "gitrepository"):
			got = cmd
			return "True\n", 0
		default:
			return "", 1
		}
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	status, err := p.GitRepositoryStatus(context.Background(), "overlay")
	require.NoError(t, err)
	assert.Equal(t, "True", status)
	assert.Contains(t, got, "gitrepository")
	assert.Contains(t, got, "flux-system")
	assert.Contains(t, got, "overlay")
}

func TestFluxer_GitRepositoryStatus_NotPresent(t *testing.T) {
	addr, cfg, cleanup := startFakeSSH(t, func(cmd string) (string, int) {
		// kubectl errors (CR not present yet) => tolerated as empty.
		return "", 1
	})
	defer cleanup()
	p := newProvisioner(t, addr, cfg)
	defer p.Close()
	status, err := p.GitRepositoryStatus(context.Background(), "overlay")
	require.NoError(t, err, "a missing/not-ready GitRepository is tolerated, not an error")
	assert.Empty(t, status)
}
