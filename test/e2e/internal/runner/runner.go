// Package runner provides the composable execution model for forge E2E tests.
package runner

import "testing"

// Scenario is an independently selectable E2E contract. Scenarios are exposed
// as subtests of the single top-level TestE2E entrypoint.
type Scenario struct {
	Name string
	Run  func(*testing.T)
}

// RunScenarios registers scenarios as named subtests. The go test -run filter
// selects scenarios without each suite needing its own top-level Test function.
func RunScenarios(t *testing.T, scenarios ...Scenario) {
	t.Helper()
	seen := make(map[string]struct{}, len(scenarios))
	for _, scenario := range scenarios {
		if scenario.Name == "" {
			t.Fatal("E2E scenario name must not be empty")
		}
		if scenario.Run == nil {
			t.Fatalf("E2E scenario %q has no run function", scenario.Name)
		}
		if _, ok := seen[scenario.Name]; ok {
			t.Fatalf("duplicate E2E scenario %q", scenario.Name)
		}
		seen[scenario.Name] = struct{}{}
		t.Run(scenario.Name, scenario.Run)
	}
}

// Stage is one named operation in a scenario. State is shared explicitly by
// the composed stages, while each stage receives its own testing subtest.
type Stage[S any] struct {
	Name string
	Run  func(*testing.T, S)
}

// RunStages executes stages in order and stops after the first failed stage.
// Later stages depend on the state established by earlier stages, so continuing
// would usually produce misleading secondary failures. Cleanup registered by
// the parent scenario still runs.
func RunStages[S any](t *testing.T, state S, stages ...Stage[S]) {
	t.Helper()
	seen := make(map[string]struct{}, len(stages))
	for _, stage := range stages {
		if stage.Name == "" {
			t.Fatal("E2E stage name must not be empty")
		}
		if stage.Run == nil {
			t.Fatalf("E2E stage %q has no run function", stage.Name)
		}
		if _, ok := seen[stage.Name]; ok {
			t.Fatalf("duplicate E2E stage %q", stage.Name)
		}
		seen[stage.Name] = struct{}{}
		if ok := t.Run(stage.Name, func(t *testing.T) { stage.Run(t, state) }); !ok {
			return
		}
	}
}
