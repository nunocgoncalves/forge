package e2e

import (
	"testing"

	"github.com/nunocgoncalves/forge/test/e2e/internal/runner"
)

// TestE2E is the suite's single entrypoint. Scenarios remain independently
// selectable with go test -run while sharing one runner and stage model.
func TestE2E(t *testing.T) {
	runner.RunScenarios(t,
		runner.Scenario{Name: "digitalocean-cpu", Run: runDigitalOceanCPU},
		runner.Scenario{Name: "digitalocean-gpu", Run: runDigitalOceanGPU},
		runner.Scenario{Name: "kind-controlplane-identity", Run: runControlPlaneIdentity},
		runner.Scenario{Name: "kind-inference-contract", Run: runInferenceFlowContract},
		runner.Scenario{Name: "kind-cert-issuers", Run: runCertIssuers},
		runner.Scenario{Name: "kind-internal-tls", Run: runInternalTLS},
	)
}
