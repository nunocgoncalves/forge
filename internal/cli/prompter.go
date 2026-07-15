package cli

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/term"
)

// passwordPrompter reads a secret/token value from stdin with echo off. The real
// impl reads from the terminal; tests use a fake. Shared by the overlay git
// token + the secret-sync value resolution so the terminal read lives in one
// place.
type passwordPrompter interface {
	Prompt(label string) ([]byte, error)
}

// termPasswordPrompter reads a value from stdin with echo off (golang.org/x/term).
type termPasswordPrompter struct{}

func (termPasswordPrompter) Prompt(label string) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "%s (non-echo): ", label)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	return b, err
}

// isTTY reports whether stdin is a terminal (gates the interactive prompt path
// for the overlay git token + secret values). Uses term.IsTerminal so /dev/null
// — a CharDevice but not a terminal, e.g. a subprocess's default stdin in CI —
// is correctly treated as non-interactive (no prompt).
func isTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
