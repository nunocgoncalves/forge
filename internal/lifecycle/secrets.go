package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nunocgoncalves/forge/internal/deployer"
	"github.com/nunocgoncalves/forge/internal/overlayer"
)

// SecretSync declares a Kubernetes Secret forge materializes from an
// operator-local env var. The declaration (name/namespace/key/envVar) is
// non-secret and lives in the overlay repo's secrets.yaml (the runtime GitOps
// source of truth); the VALUE is read from the named env var (gitignored .env)
// and applied via `kubectl apply -f -` over SSH stdin — it never touches helm
// values, git, or the process list. First consumer: the Cloudflare API token
// shared by cert-manager (cert-issuers, HOR-342) + external-dns (HOR-343).
type SecretSync struct {
	Name      string `yaml:"name"`      // Secret name
	Namespace string `yaml:"namespace"` // target namespace (must match the consumer's ns)
	Key       string `yaml:"key"`       // data key
	EnvVar    string `yaml:"envVar"`    // operator env var holding the value
	Type      string `yaml:"type"`      // optional; defaults to Opaque
}

// secretsFile is the shape of the overlay's secrets.yaml.
type secretsFile struct {
	Secrets []SecretSync `yaml:"secrets"`
}

// SecretResolver resolves a declared secret's value. The CLI provides the
// terminal implementation (env var wins + is logged; otherwise prompt on TTY;
// otherwise error); tests inject fakes. Mirrors the overlay git-token
// resolution, adapted for declarations discovered at apply time from the
// overlay's secrets.yaml.
type SecretResolver interface {
	Resolve(ctx context.Context, name, envVar string) (string, error)
}

// envOnlySecretResolver is the fallback when no resolver is wired: env var only,
// error if unset. Used when applySecrets runs without a CLI resolver (e.g. tests
// that don't exercise the prompt path).
type envOnlySecretResolver struct{}

func (envOnlySecretResolver) Resolve(_ context.Context, name, envVar string) (string, error) {
	v, ok := os.LookupEnv(envVar)
	if !ok {
		return "", fmt.Errorf("secret %q: env var %q is unset (set it in the operator's gitignored .env)", name, envVar)
	}
	return v, nil
}

// applySecrets is the secret-sync phase: it reads the overlay's secrets.yaml
// (non-secret declarations) from the cloned overlay on the host, resolves each
// env var on the operator, and materializes the Secrets via `kubectl apply -f -`
// over SSH stdin (Deployer.ApplyManifest). Runs AFTER the overlay clone + BEFORE
// the platform chart so consumers (cert-manager's ClusterIssuer) find the Secret
// on first reconcile.
//
// Invariants: secret values are read from env vars (gitignored .env) and piped
// via stdin — they never appear in a command string, ps, helm values, or git.
// The declaration is the only thing in the overlay. Idempotent (kubectl apply);
// reality-as-state on re-apply. No-op without an overlay (secrets live there).
func applySecrets(ctx context.Context, o overlayer.Overlayer, d deployer.Deployer, opts ApplyOpts, res *Result, overlayDest string) error {
	if d == nil || o == nil || opts.SkipSecrets || overlayDest == "" {
		return nil
	}

	// secrets.yaml is optional: an overlay without it simply has no secrets to
	// materialize. A missing file surfaces as a "No such file" cat error.
	content, err := o.ReadFile(ctx, overlayDest, "secrets.yaml")
	if err != nil {
		if strings.Contains(err.Error(), "No such file") {
			return nil // no secrets.yaml ⇒ no secrets
		}
		return fmt.Errorf("read overlay secrets.yaml: %w", err)
	}

	secs, err := parseSecrets([]byte(content))
	if err != nil {
		return fmt.Errorf("parse overlay secrets.yaml: %w", err)
	}
	if len(secs) == 0 {
		return nil
	}

	if err := ensureSecretNamespaces(ctx, d, secs); err != nil {
		return err
	}
	resolver := opts.SecretResolver
	if resolver == nil {
		resolver = envOnlySecretResolver{}
	}
	for _, s := range secs {
		if err := materializeSecret(ctx, d, s, resolver); err != nil {
			return err
		}
	}
	res.SecretsApplied = true
	return nil
}

// ensureSecretNamespaces idempotently creates each secret's namespace. The
// chart would create the release namespace via --create-namespace, but secrets
// run before the chart so the namespace must pre-exist.
func ensureSecretNamespaces(ctx context.Context, d deployer.Deployer, secs []SecretSync) error {
	seen := map[string]bool{}
	for _, s := range secs {
		if s.Namespace == "" {
			return fmt.Errorf("secret declaration missing namespace")
		}
		if seen[s.Namespace] {
			continue
		}
		seen[s.Namespace] = true
		if err := d.ApplyManifest(ctx, namespaceManifestJSON(s.Namespace)); err != nil {
			return fmt.Errorf("secret namespace %q: %w", s.Namespace, err)
		}
	}
	return nil
}

// materializeSecret validates a declaration, resolves its value via the
// resolver, and applies the Secret manifest via stdin.
func materializeSecret(ctx context.Context, d deployer.Deployer, s SecretSync, r SecretResolver) error {
	if err := s.validate(); err != nil {
		return err
	}
	val, err := r.Resolve(ctx, s.Name, s.EnvVar)
	if err != nil {
		return err
	}
	if err := d.ApplyManifest(ctx, secretManifestJSON(s, val)); err != nil {
		return fmt.Errorf("secret %q: %w", s.Name, err)
	}
	return nil
}

// parseSecrets parses the overlay's secrets.yaml and applies the Opaque default.
func parseSecrets(data []byte) ([]SecretSync, error) {
	var sf secretsFile
	if err := yaml.Unmarshal(data, &sf); err != nil {
		return nil, err
	}
	for i := range sf.Secrets {
		if sf.Secrets[i].Type == "" {
			sf.Secrets[i].Type = "Opaque"
		}
	}
	return sf.Secrets, nil
}

func (s SecretSync) validate() error {
	if s.Name == "" {
		return fmt.Errorf("secret declaration missing name")
	}
	if s.Key == "" {
		return fmt.Errorf("secret %q: missing key", s.Name)
	}
	if s.EnvVar == "" {
		return fmt.Errorf("secret %q: missing envVar", s.Name)
	}
	return nil
}

// namespaceManifest is a minimal Namespace manifest applied to ensure a secret's
// namespace exists before the chart (and before the secret itself).
type namespaceManifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// secretManifest is a Secret manifest with the value in stringData (plaintext;
// kubectl stores it base64-encoded in .data). JSON is used so arbitrary values
// are escaped safely without a YAML dependency; kubectl apply -f - accepts JSON.
type secretManifest struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Type       string            `json:"type"`
	Metadata   secretMeta        `json:"metadata"`
	StringData map[string]string `json:"stringData"`
}

type secretMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// namespaceManifestJSON renders a Namespace manifest as JSON.
func namespaceManifestJSON(name string) string {
	var m namespaceManifest
	m.APIVersion = "v1"
	m.Kind = "Namespace"
	m.Metadata.Name = name
	b, _ := json.Marshal(m)
	return string(b)
}

// secretManifestJSON renders a Secret manifest (stringData) as JSON. The value
// is embedded in the manifest, which is piped via stdin to kubectl — never in a
// command string.
func secretManifestJSON(s SecretSync, value string) string {
	m := secretManifest{
		APIVersion: "v1",
		Kind:       "Secret",
		Type:       s.Type,
		Metadata:   secretMeta{Name: s.Name, Namespace: s.Namespace},
		StringData: map[string]string{s.Key: value},
	}
	b, _ := json.Marshal(m)
	return string(b)
}
