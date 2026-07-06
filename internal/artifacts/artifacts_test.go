package artifacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteReadKubeconfig(t *testing.T) {
	t.Setenv(envRoot, t.TempDir())
	require.NoError(t, WriteKubeconfig("opo1", []byte("apiVersion: v1\n")))
	got, err := ReadKubeconfig("opo1")
	require.NoError(t, err)
	assert.Equal(t, "apiVersion: v1\n", string(got))

	// file mode is 0600
	p, err := KubeconfigPath("opo1")
	require.NoError(t, err)
	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestAppendAudit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envRoot, dir)
	require.NoError(t, AppendAudit("opo1", AuditRecord{Action: "apply", Result: "success"}))
	require.NoError(t, AppendAudit("opo1", AuditRecord{Action: "upgrade", Result: "failure", Detail: "boom"}))

	data, err := os.ReadFile(filepath.Join(dir, "opo1", "audit.jsonl"))
	require.NoError(t, err)

	lines := splitLines(string(data))
	require.Len(t, lines, 2)

	var r1, r2 AuditRecord
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &r1))
	assert.Equal(t, "apply", r1.Action)
	assert.Equal(t, "success", r1.Result)
	assert.False(t, r1.Time.IsZero())

	require.NoError(t, json.Unmarshal([]byte(lines[1]), &r2))
	assert.Equal(t, "upgrade", r2.Action)
	assert.Equal(t, "boom", r2.Detail)
}

func TestRoot_DefaultHome(t *testing.T) {
	t.Setenv(envRoot, "")
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	got, err := Root()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".forge"), got)
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
