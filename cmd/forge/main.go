// Command forge provisions and manages single-node k3s clusters.
package main

import (
	"fmt"
	"os"

	"github.com/nunocgoncalves/forge/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
