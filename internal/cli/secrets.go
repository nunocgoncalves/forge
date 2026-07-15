package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/term"
)

// secretPrompter reads a secret value from stdin with echo off. The real impl
// reads from the terminal; tests use a fake.
type secretPrompter interface {
	Prompt(label string) ([]byte, error)
}

type termSecretPrompter struct{}

func (termSecretPrompter) Prompt(label string) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "%s (non-echo): ", label)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	return b, err
}

// resolveSecretValue determines a declared secret's value, mirroring
// resolveOverlayToken's env-first / prompt-on-TTY behavior:
//
//   - an operator env var wins (and is reported on `out` so the user sees why the
//     prompt is skipped);
//   - otherwise an interactive operator is prompted (non-echo);
//   - otherwise (CI / non-interactive + no env var) it is an error — a declared
//     secret is required, unlike the optional overlay git token (which proceeds
//     tokenless for a public repo).
func resolveSecretValue(name, envVar string, interactive bool, prompter secretPrompter, out io.Writer) (string, error) {
	if v, ok := os.LookupEnv(envVar); ok {
		fmt.Fprintf(out, "secret %q: env var %s set — using it (skipping prompt)\n", name, envVar)
		return v, nil
	}
	if !interactive || prompter == nil {
		return "", fmt.Errorf("secret %q: env var %s is unset and stdin is not a TTY (set it in the operator's gitignored .env, or run forge interactively to prompt)", name, envVar)
	}
	b, err := prompter.Prompt(fmt.Sprintf("secret %q value (env var %s)", name, envVar))
	if err != nil {
		return "", fmt.Errorf("secret %q: read value: %w", name, err)
	}
	return string(b), nil
}

// cliSecretResolver adapts resolveSecretValue to lifecycle.SecretResolver. The
// CLI constructs it with the TTY state + a terminal prompter; stderr for the
// "env detected" notice.
type cliSecretResolver struct {
	interactive bool
	prompter    secretPrompter
	out         io.Writer
}

// Resolve implements lifecycle.SecretResolver.
func (r cliSecretResolver) Resolve(_ context.Context, name, envVar string) (string, error) {
	return resolveSecretValue(name, envVar, r.interactive, r.prompter, r.out)
}
