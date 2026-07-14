// Package overlayer defines the host-level overlay-repo git operations
// interface — the testability seam for cloning/removing the client overlay fork
// on the host. The real implementation lives in internal/sshprovisioner (git
// runs on the host over SSH, sharing the same transport as the k3s Provisioner
// + Helm Deployer); tests use fakes. Lifecycle logic never talks to SSH or git
// directly — it orchestrates against this interface.
package overlayer

import "context"

// Overlayer abstracts host-level overlay-repo git operations. One instance is
// bound to the same host as the Provisioner/Deployer; git runs there over SSH.
type Overlayer interface {
	// EnsureGit ensures git is present on the host (installs it if absent,
	// mirroring ensureHelm). Idempotent. v1: Ubuntu/apt hosts.
	EnsureGit(ctx context.Context) error
	// Clone checks out the overlay repo at ref into dest (a fresh clone: dest is
	// removed first if it exists). For https repos, token is injected via a git
	// credential helper (a temp cred file written over SSH stdin) — never in the
	// command string or ps — and deleted after; the remote URL in .git/config
	// never contains the token. Pass nil for a public or file:// repo. Returns
	// the resolved commit SHA. Idempotent (re-clones to the current ref on each
	// apply — reality-as-state).
	Clone(ctx context.Context, repo, ref, dest string, token []byte) (commit string, err error)
	// Remove deletes the cloned overlay directory (destroy / re-clone).
	// Best-effort: a missing dir is not an error.
	Remove(ctx context.Context, dest string) error
	// ReadFile reads a file from the cloned overlay on the host (e.g. secrets.yaml).
	// A missing file returns an error whose message contains "No such file" so the
	// caller can treat an optional file (like secrets.yaml) as absent. Used by the
	// secret-sync phase to read the overlay's non-secret secret declarations.
	ReadFile(ctx context.Context, dest, relPath string) (string, error)
}
