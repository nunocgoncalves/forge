package sshprovisioner

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/forge/internal/overlayer"
)

// Compile-time assertion: SSHProvisioner implements overlayer.Overlayer.
var _ overlayer.Overlayer = (*SSHProvisioner)(nil)

// EnsureGit implements overlayer.Overlayer. Installs git on the host if absent
// (apt; v1: Ubuntu hosts), mirroring ensureHelm. Idempotent.
func (p *SSHProvisioner) EnsureGit(ctx context.Context) error {
	if _, err := p.run(ctx, "command -v git"); err == nil {
		return nil
	}
	if _, err := p.run(ctx, "sudo apt-get update -qq && sudo apt-get install -y git"); err != nil {
		return fmt.Errorf("install git: %w", err)
	}
	return nil
}

// Clone implements overlayer.Overlayer. Fresh shallow clone of the overlay repo
// at ref into dest. For https repos with a token, the token is injected via a
// git credential-helper "store" file (written over SSH stdin so it never appears
// in the command string or ps), then deleted; the remote URL in .git/config
// never contains the token. Returns the resolved commit SHA.
func (p *SSHProvisioner) Clone(ctx context.Context, repo, ref, dest string, token []byte) (string, error) {
	// Fresh clone: remove dest if it exists, ensure the parent dir is present.
	if _, err := p.run(ctx, "rm -rf "+shellQuote(dest)); err != nil {
		return "", fmt.Errorf("overlay remove: %w", err)
	}
	if _, err := p.run(ctx, "mkdir -p "+shellQuote(filepath.Dir(dest))); err != nil {
		return "", fmt.Errorf("overlay mkdir: %w", err)
	}

	cloneArgs := []string{"git", "clone", "--branch", ref, "--depth", "1", repo, dest}
	if len(token) > 0 && strings.HasPrefix(repo, "https://") {
		credFile := dest + ".creds"
		credLine := fmt.Sprintf("https://x-access-token:%s@%s\n", string(token), httpsHost(repo))
		// Write the cred file via stdin so the token is never in the command
		// string (ps/history). umask 077 => 0600.
		if _, err := p.runStdin(ctx, "umask 077 && cat > "+shellQuote(credFile), credLine); err != nil {
			return "", fmt.Errorf("overlay credentials: %w", err)
		}
		defer func() { _, _ = p.run(ctx, "rm -f "+shellQuote(credFile)) }()
		cloneArgs = append([]string{"git", "-c", "credential.helper=store --file=" + credFile},
			cloneArgs[1:]...) // replace the leading "git"
	}

	if _, err := p.run(ctx, joinArgs(cloneArgs)); err != nil {
		return "", fmt.Errorf("overlay clone: %w", err)
	}

	// Light structural validation: the overlay must have the files forge consumes.
	// Fails fast with an actionable error instead of an opaque helm/kubectl failure.
	if err := p.validateOverlayStructure(ctx, dest); err != nil {
		_ = p.Remove(ctx, dest) // clean up the malformed clone
		return "", err
	}

	commit, err := p.run(ctx, "git -C "+shellQuote(dest)+" rev-parse HEAD")
	if err != nil {
		return "", fmt.Errorf("overlay commit: %w", err)
	}
	return strings.TrimSpace(commit), nil
}

// Remove implements overlayer.Overlayer. Best-effort: a missing dir is not an error.
func (p *SSHProvisioner) Remove(ctx context.Context, dest string) error {
	if _, err := p.run(ctx, "rm -rf "+shellQuote(dest)); err != nil {
		return fmt.Errorf("overlay remove: %w", err)
	}
	return nil
}

// httpsHost extracts the host[:port] from an https:// URL (no path), used to
// scope the credential-helper cred line to the repo's host.
func httpsHost(repo string) string {
	s := strings.TrimPrefix(repo, "https://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// validateOverlayStructure checks the cloned overlay has the files forge
// consumes (values.yaml, values.client.yaml, crds/client/kustomization.yaml).
func (p *SSHProvisioner) validateOverlayStructure(ctx context.Context, dest string) error {
	check := "test -f " + shellQuote(dest+"/values.yaml") +
		" && test -f " + shellQuote(dest+"/values.client.yaml") +
		" && test -f " + shellQuote(dest+"/crds/client/kustomization.yaml")
	if _, err := p.run(ctx, check); err != nil {
		return fmt.Errorf("overlay structure: %s is missing values.yaml, values.client.yaml, or crds/client/kustomization.yaml", dest)
	}
	return nil
}

// runStdin runs cmd on the host with stdin supplied. The command string itself
// never contains the stdin data, so secrets can be written to files without
// appearing in ps/history. Mirrors run's context + session handling.
func (p *SSHProvisioner) runStdin(ctx context.Context, cmd, stdin string) (string, error) {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return "", err
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	sess.Stdin = strings.NewReader(stdin)
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
