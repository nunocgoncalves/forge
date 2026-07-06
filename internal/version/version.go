// Package version holds build-time version metadata injected via ldflags.
package version

import (
	"fmt"
	"runtime"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// String returns a human-readable version line.
func String() string {
	return fmt.Sprintf("forge %s (commit: %s, built: %s, %s)", version, commit, date, runtime.Version())
}
