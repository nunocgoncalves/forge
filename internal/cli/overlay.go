package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// TokenPrompter reads an overlay git token with echo off. The real impl reads
// from the terminal; tests use a fake.
type TokenPrompter interface {
	Prompt() ([]byte, error)
}

// termPrompter reads a token from stdin with echo off (golang.org/x/term).
type termPrompter struct{}

func (termPrompter) Prompt() ([]byte, error) {
	fmt.Fprint(os.Stderr, "Overlay repo token (non-echo; enter for a public repo): ")
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	return b, err
}

// ScopeChecker validates an overlay git token has access to the repo's host
// (GitHub PAT scope check). The real impl calls the GitHub API; tests use a fake.
type ScopeChecker interface {
	Check(ctx context.Context, token []byte, repo string) error
}

// githubScopeChecker validates a GitHub token via GET /user (401 => invalid) and
// warns if the classic-PAT X-OAuth-Scopes header lacks "repo". Non-GitHub repos
// skip the check (returns nil). A missing "repo" scope is a warning, not an
// error — the clone enforces actual repo access.
type githubScopeChecker struct{ client *http.Client }

func (g githubScopeChecker) Check(ctx context.Context, token []byte, repo string) error {
	if !strings.HasPrefix(repo, "https://github.com/") {
		return nil // scope check is GitHub-only
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return fmt.Errorf("token scope check: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("token scope check: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("overlay token is invalid or expired (GitHub returned 401)")
	case resp.StatusCode >= 400:
		return fmt.Errorf("overlay token scope check: GitHub returned %d", resp.StatusCode)
	}
	if scopes := resp.Header.Get("X-OAuth-Scopes"); scopes != "" && !strings.Contains(scopes, "repo") {
		fmt.Fprintf(os.Stderr, "warning: overlay token scopes [%s] lack 'repo'; a private overlay clone may fail\n", scopes)
	}
	return nil
}

// resolveOverlayToken determines the overlay git token:
//   - FORGE_OVERLAY_TOKEN env var (non-interactive) wins; its scopes are checked.
//   - Otherwise, for an https repo when interactive (TTY), prompt (non-echo;
//     empty => public repo, tokenless). A prompted token's scopes are checked.
//   - file:// repos, or https with no env var + non-interactive (CI), need no
//     token (public/CI proceeds tokenless).
func resolveOverlayToken(ctx context.Context, repo, envToken string, interactive bool, tp TokenPrompter, sc ScopeChecker) ([]byte, error) {
	if envToken != "" {
		tok := []byte(envToken)
		if err := sc.Check(ctx, tok, repo); err != nil {
			return nil, err
		}
		return tok, nil
	}
	if !strings.HasPrefix(repo, "https://") {
		return nil, nil // file:// needs no token
	}
	if !interactive || tp == nil {
		return nil, nil // CI / non-interactive proceeds tokenless
	}
	tok, err := tp.Prompt()
	if err != nil {
		return nil, fmt.Errorf("read overlay token: %w", err)
	}
	if len(tok) == 0 {
		return nil, nil // empty => public repo, tokenless
	}
	if err := sc.Check(ctx, tok, repo); err != nil {
		return nil, err
	}
	return tok, nil
}

// isTTY reports whether stdin is a terminal (gates the interactive token prompt).
func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// newGithubScopeChecker builds the production GitHub scope checker.
func newGithubScopeChecker() ScopeChecker {
	return githubScopeChecker{client: &http.Client{Timeout: 15 * time.Second}}
}
