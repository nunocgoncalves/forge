// Package artifacts manages the local ~/.forge/<install>/ state directory
// (fetched kubeconfig + audit log). It is NOT authoritative cluster state —
// the live system is the source of truth; this holds operational artifacts
// only (the kubeconfig is re-fetchable from the host at any time).
package artifacts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// envRoot overrides the state root (used by tests; also lets users relocate
// ~/.forge).
const envRoot = "FORGE_HOME"

// AuditRecord is one entry in the per-install audit log.
type AuditRecord struct {
	Time    time.Time `json:"time"`
	Action  string    `json:"action"`            // apply | upgrade | destroy | ...
	Version string    `json:"version,omitempty"` // forge version
	Result  string    `json:"result"`            // success | failure
	Detail  string    `json:"detail,omitempty"`
}

// Root returns the forge state root (FORGE_HOME env or ~/.forge).
func Root() (string, error) {
	if v := os.Getenv(envRoot); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home dir: %w", err)
	}
	return filepath.Join(home, ".forge"), nil
}

// Dir returns the per-install directory.
func Dir(name string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// EnsureDir creates the per-install directory (0700).
func EnsureDir(name string) error {
	d, err := Dir(name)
	if err != nil {
		return err
	}
	return os.MkdirAll(d, 0o700)
}

// KubeconfigPath returns the path to the stored kubeconfig.
func KubeconfigPath(name string) (string, error) {
	d, err := Dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "kubeconfig.yaml"), nil
}

// WriteKubeconfig writes the kubeconfig (0600).
func WriteKubeconfig(name string, data []byte) error {
	if err := EnsureDir(name); err != nil {
		return err
	}
	p, err := KubeconfigPath(name)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// ReadKubeconfig reads the stored kubeconfig.
func ReadKubeconfig(name string) ([]byte, error) {
	p, err := KubeconfigPath(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// AppendAudit appends a record to the per-install audit.jsonl.
func AppendAudit(name string, r AuditRecord) error {
	if err := EnsureDir(name); err != nil {
		return err
	}
	d, err := Dir(name)
	if err != nil {
		return err
	}
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(filepath.Join(d, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}
