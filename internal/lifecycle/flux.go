package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/nunocgoncalves/forge/internal/config"
	"github.com/nunocgoncalves/forge/internal/deployer"
	"github.com/nunocgoncalves/forge/internal/fluxer"
)

// Flux sync resource names + constants. v1 single-node => one install per
// cluster, so fixed names in the flux-system namespace are unambiguous.
const (
	fluxNamespace       = "flux-system"
	fluxSourceName      = "overlay"          // GitRepository name (source-controller fetches the fork)
	fluxKustomizeName   = "overlay-crds"     // Kustomization name (reconciles crds/client)
	fluxTokenSecretName = "overlay-git-auth" //nolint:gosec // resource name, not a credential (GitRepository secretRef)
	fluxGitUsername     = "git"              // generic https username (GitHub ignores it; password is the PAT)
	fluxInterval        = "1m"               // GitRepository + Kustomization poll interval
	fluxCRDPath         = "./crds/client"    // Kustomization path (the overlay's CRD instances)
)

// semverTagRe matches a flux2-style version tag (vX.Y.Z[-pre]); used to decide
// whether overlay.ref is a tag (Flux ref.tag) or a branch (Flux ref.branch).
// forge's overlay.ref accepts both (git clone --branch works for either); Flux's
// GitRepository distinguishes the two.
var semverTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z-.]+)?$`)

// applyFluxPhase runs the Flux GitOps phase: install Flux components
// (EnsureFlux, which also installs the source/kustomize CRDs) → token Secret →
// GitRepository → Kustomization. Flux then continuously reconciles the overlay's
// CRD instances (crds/client, prune=true) + materializes the fork in-cluster via
// source-controller (the pi/ tree source for the AgentSandbox operator,
// HOR-351). Runs LAST (substrate + one-time overlay before continuous). No-op
// when Flux is disabled or skipped; never gates apply on Flux reconcile health
// (Flux reconciles async — the status read is informational).
func applyFluxPhase(ctx context.Context, cfg *config.Cluster, f fluxer.Fluxer, d deployer.Deployer, opts ApplyOpts, res *Result) error {
	if !cfg.Spec.Flux.Enabled || opts.SkipFlux {
		return nil
	}
	if f == nil {
		return fmt.Errorf("flux.enabled is set but no fluxer is wired (internal error)")
	}
	if err := f.EnsureFlux(ctx, cfg.Spec.Flux.Version); err != nil {
		auditFail(cfg, "apply-flux", err)
		return fmt.Errorf("flux install: %w", err)
	}

	// Token Secret (only when a token was resolved — public repos omit it and
	// Flux clones anonymously). Applied via stdin (kubectl apply -f -) so the
	// token never appears in a command string or ps; mirrors the secret-sync
	// phase invariant.
	hasToken := len(opts.OverlayToken) > 0
	if hasToken {
		sec := fluxTokenSecretManifest(fluxTokenSecretName, fluxNamespace, fluxGitUsername, opts.OverlayToken)
		if err := d.ApplyManifest(ctx, sec); err != nil {
			auditFail(cfg, "apply-flux", err)
			return fmt.Errorf("flux token secret: %w", err)
		}
	}

	// GitRepository: source-controller fetches + materializes the client fork.
	secretRef := ""
	if hasToken {
		secretRef = fluxTokenSecretName
	}
	repo := gitRepositoryManifest(fluxSourceName, fluxNamespace, cfg.Spec.Overlay.Repo, cfg.Spec.Overlay.Ref, secretRef)
	if err := d.ApplyManifest(ctx, repo); err != nil {
		auditFail(cfg, "apply-flux", err)
		return fmt.Errorf("flux gitrepository: %w", err)
	}

	// Kustomization: reconcile crds/client (prune=true — Flux is the mirror
	// authority, including deletions; forge's one-time apply -k is the immediate
	// convergence layer but does not prune).
	kust := kustomizationManifest(fluxKustomizeName, fluxNamespace, fluxSourceName, fluxCRDPath)
	if err := d.ApplyManifest(ctx, kust); err != nil {
		auditFail(cfg, "apply-flux", err)
		return fmt.Errorf("flux kustomization: %w", err)
	}

	res.FluxInstalled = true
	// Best-effort status read (informational; never gates apply). Flux reconciles
	// async — the GitRepository may not be Ready yet on first apply, and git
	// egress from the cluster is not forge's responsibility to gate on.
	if status, err := f.GitRepositoryStatus(ctx, fluxSourceName); err == nil {
		res.GitRepositoryStatus = status
	}
	return nil
}

// fluxMeta is the shared metadata block for the Flux sync resources.
type fluxMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// fluxTokenSecret is a Secret holding the overlay git token (stringData) for
// Flux's GitRepository secretRef. Marshaled to JSON + piped via stdin (kubectl
// apply -f -) so the token never appears in a command string or ps — mirrors
// secretManifest in secrets.go.
type fluxTokenSecret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Type       string            `json:"type"`
	Metadata   fluxMeta          `json:"metadata"`
	StringData map[string]string `json:"stringData"`
}

// fluxTokenSecretManifest renders the token Secret as JSON. The token is in
// stringData (kubectl stores it base64-encoded in .data).
func fluxTokenSecretManifest(name, namespace, username string, password []byte) string {
	m := fluxTokenSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Type:       "Opaque",
		Metadata:   fluxMeta{Name: name, Namespace: namespace},
		StringData: map[string]string{"username": username, "password": string(password)},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// gitRepository is a Flux source.toolkit.fluxcd.io/v1 GitRepository.
type gitRepository struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   fluxMeta    `json:"metadata"`
	Spec       gitRepoSpec `json:"spec"`
}

type gitRepoSpec struct {
	URL       string     `json:"url"`
	Ref       gitRepoRef `json:"ref"`
	Interval  string     `json:"interval"`
	SecretRef *fluxRef   `json:"secretRef,omitempty"`
}

// gitRepoRef selects branch or tag. A semver-looking ref (vX.Y.Z[-pre]) is a
// tag; otherwise a branch. (Flux also supports ref.semver/commit, not needed in
// v1.)
type gitRepoRef struct {
	Branch string `json:"branch,omitempty"`
	Tag    string `json:"tag,omitempty"`
}

type fluxRef struct {
	Name string `json:"name"`
}

// gitRepositoryManifest renders the GitRepository as JSON. secretRef is omitted
// (nil) for a public repo — Flux clones anonymously.
func gitRepositoryManifest(name, namespace, url, ref, secretRef string) string {
	r := gitRepository{
		APIVersion: "source.toolkit.fluxcd.io/v1",
		Kind:       "GitRepository",
		Metadata:   fluxMeta{Name: name, Namespace: namespace},
		Spec: gitRepoSpec{
			URL:      url,
			Ref:      fluxRefFor(ref),
			Interval: fluxInterval,
		},
	}
	if secretRef != "" {
		r.Spec.SecretRef = &fluxRef{Name: secretRef}
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// fluxRefFor maps overlay.ref to a Flux GitRepository ref: a semver tag
// (vX.Y.Z[-pre]) → ref.tag, else ref.branch.
func fluxRefFor(ref string) gitRepoRef {
	if semverTagRe.MatchString(ref) {
		return gitRepoRef{Tag: ref}
	}
	return gitRepoRef{Branch: ref}
}

// kustomization is a Flux kustomize.toolkit.fluxcd.io/v1 Kustomization.
type kustomization struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   fluxMeta          `json:"metadata"`
	Spec       kustomizationSpec `json:"spec"`
}

type kustomizationSpec struct {
	SourceRef kustomizationSource `json:"sourceRef"`
	Path      string              `json:"path"`
	Prune     bool                `json:"prune"`
	Wait      bool                `json:"wait"`
	Interval  string              `json:"interval"`
}

type kustomizationSource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// kustomizationManifest renders the Kustomization as JSON. prune=true makes Flux
// the mirror authority (removals from the repo are GC'd); wait=true gates
// reconcile on health.
func kustomizationManifest(name, namespace, sourceName, path string) string {
	k := kustomization{
		APIVersion: "kustomize.toolkit.fluxcd.io/v1",
		Kind:       "Kustomization",
		Metadata:   fluxMeta{Name: name, Namespace: namespace},
		Spec: kustomizationSpec{
			SourceRef: kustomizationSource{Kind: "GitRepository", Name: sourceName},
			Path:      path,
			Prune:     true,
			Wait:      true,
			Interval:  fluxInterval,
		},
	}
	b, _ := json.Marshal(k)
	return string(b)
}
