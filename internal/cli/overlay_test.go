package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePrompter struct {
	token []byte
	err   error
	calls int
}

func (f *fakePrompter) Prompt() ([]byte, error) {
	f.calls++
	return f.token, f.err
}

type fakeScopeChecker struct {
	err      error
	gotToken []byte
	gotRepo  string
	calls    int
}

func (f *fakeScopeChecker) Check(_ context.Context, token []byte, repo string) error {
	f.calls++
	f.gotToken = token
	f.gotRepo = repo
	return f.err
}

func TestResolveOverlayToken_EnvVar(t *testing.T) {
	sc := &fakeScopeChecker{}
	tok, err := resolveOverlayToken(context.Background(), "https://github.com/example/overlay.git", "ghp_env", true, &fakePrompter{}, sc)
	require.NoError(t, err)
	assert.Equal(t, []byte("ghp_env"), tok)
	assert.Equal(t, 1, sc.calls, "scope checked for env token")
	assert.Equal(t, "https://github.com/example/overlay.git", sc.gotRepo)
}

func TestResolveOverlayToken_EnvVarScopeError(t *testing.T) {
	sc := &fakeScopeChecker{err: errors.New("401 invalid")}
	_, err := resolveOverlayToken(context.Background(), "https://github.com/example/overlay.git", "ghp_env", true, nil, sc)
	require.Error(t, err)
}

func TestResolveOverlayToken_PromptInteractive(t *testing.T) {
	sc := &fakeScopeChecker{}
	tp := &fakePrompter{token: []byte("ghp_prompt")}
	tok, err := resolveOverlayToken(context.Background(), "https://github.com/example/overlay.git", "", true, tp, sc)
	require.NoError(t, err)
	assert.Equal(t, []byte("ghp_prompt"), tok)
	assert.Equal(t, 1, tp.calls)
	assert.Equal(t, 1, sc.calls, "scope checked for prompted token")
}

func TestResolveOverlayToken_PromptEmptyIsPublic(t *testing.T) {
	sc := &fakeScopeChecker{}
	tp := &fakePrompter{token: nil} // empty prompt => public
	tok, err := resolveOverlayToken(context.Background(), "https://github.com/example/overlay.git", "", true, tp, sc)
	require.NoError(t, err)
	assert.Nil(t, tok, "empty prompt => tokenless (public repo)")
	assert.Equal(t, 1, tp.calls)
	assert.Equal(t, 0, sc.calls, "no scope check for a tokenless public repo")
}

func TestResolveOverlayToken_NoPromptInCI(t *testing.T) {
	sc := &fakeScopeChecker{}
	tp := &fakePrompter{token: []byte("should-not-prompt")}
	tok, err := resolveOverlayToken(context.Background(), "https://github.com/example/overlay.git", "", false, tp, sc) // non-interactive
	require.NoError(t, err)
	assert.Nil(t, tok, "CI proceeds tokenless")
	assert.Equal(t, 0, tp.calls, "no prompt in CI")
	assert.Equal(t, 0, sc.calls)
}

func TestResolveOverlayToken_FileURLNoToken(t *testing.T) {
	sc := &fakeScopeChecker{}
	tp := &fakePrompter{token: []byte("x")}
	tok, err := resolveOverlayToken(context.Background(), "file:///tmp/overlay", "", true, tp, sc)
	require.NoError(t, err)
	assert.Nil(t, tok, "file:// needs no token")
	assert.Equal(t, 0, tp.calls)
	assert.Equal(t, 0, sc.calls)
}
