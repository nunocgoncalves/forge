// Command forge provisions and manages single-node k3s clusters.
package main

import (
	"os"

	"github.com/nunocgoncalves/forge/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
