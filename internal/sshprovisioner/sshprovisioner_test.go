package sshprovisioner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/forge/internal/config"
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
			return "[Unit]\nDescription=k3s\n[Service]\nExecStart=/usr/local/bin/k3s server --cluster-cidr 10.42.0.0/16,fd42::/48 --service-cidr 10.43.0.0/16,fd43::/112 --flannel-backend=vxlan --tls-san 10.20.0.10 --disable traefik --disable servicelb --write-kubeconfig-mode 0600\n[Install]\n", 0
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
