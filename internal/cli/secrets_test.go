package cli

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSecretPrompter struct {
	value    []byte
	err      error
	calls    int
	gotLabel string
}

func (f *fakeSecretPrompter) Prompt(label string) ([]byte, error) {
	f.calls++
	f.gotLabel = label
	return f.value, f.err
}

// ensureEnvUnset removes envVar for the test (so os.LookupEnv ok=false), restoring it after.
func ensureEnvUnset(t *testing.T, envVar string) {
	t.Helper()
	if old, ok := os.LookupEnv(envVar); ok {
		os.Unsetenv(envVar)
		t.Cleanup(func() { os.Setenv(envVar, old) })
	}
}

func TestResolveSecretValue_EnvVar(t *testing.T) {
	t.Setenv("FORGE_TEST_SECRET", "envval")
	var buf bytes.Buffer
	tp := &fakeSecretPrompter{value: []byte("should-not-prompt")}
	v, err := resolveSecretValue("cloudflare-api-token", "FORGE_TEST_SECRET", true, tp, &buf)
	require.NoError(t, err)
	assert.Equal(t, "envval", v)
	assert.Equal(t, 0, tp.calls, "env var set ⇒ no prompt")
	assert.Contains(t, buf.String(), "FORGE_TEST_SECRET", "env detection is logged")
	assert.Contains(t, buf.String(), "skipping prompt")
}

func TestResolveSecretValue_PromptInteractive(t *testing.T) {
	ensureEnvUnset(t, "FORGE_TEST_SECRET")
	var buf bytes.Buffer
	tp := &fakeSecretPrompter{value: []byte("prompted-val")}
	v, err := resolveSecretValue("cloudflare-api-token", "FORGE_TEST_SECRET", true, tp, &buf)
	require.NoError(t, err)
	assert.Equal(t, "prompted-val", v)
	assert.Equal(t, 1, tp.calls, "no env var + interactive ⇒ prompt")
	assert.Empty(t, buf.String(), "no env-detection log when prompting")
}

func TestResolveSecretValue_NoPromptInCI(t *testing.T) {
	ensureEnvUnset(t, "FORGE_TEST_SECRET")
	var buf bytes.Buffer
	tp := &fakeSecretPrompter{value: []byte("should-not-prompt")}
	_, err := resolveSecretValue("cloudflare-api-token", "FORGE_TEST_SECRET", false, tp, &buf) // non-interactive
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unset")
	assert.Contains(t, err.Error(), "not a TTY")
	assert.Equal(t, 0, tp.calls, "no prompt in CI")
}

func TestCliSecretResolver_Delegates(t *testing.T) {
	t.Setenv("FORGE_TEST_SECRET", "envval")
	var buf bytes.Buffer
	r := cliSecretResolver{interactive: true, prompter: termSecretPrompter{}, out: &buf}
	v, err := r.Resolve(context.Background(), "cloudflare-api-token", "FORGE_TEST_SECRET")
	require.NoError(t, err)
	assert.Equal(t, "envval", v)
	assert.Contains(t, buf.String(), "skipping prompt")
}
