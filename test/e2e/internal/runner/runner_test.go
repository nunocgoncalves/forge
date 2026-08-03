package runner

import (
	"reflect"
	"testing"
)

func TestRunStagesComposesSharedStateInOrder(t *testing.T) {
	t.Parallel()

	type state struct{ events []string }
	s := &state{}
	RunStages(t, s,
		Stage[*state]{Name: "deploy", Run: func(_ *testing.T, s *state) {
			s.events = append(s.events, "deployed")
		}},
		Stage[*state]{Name: "run-inference", Run: func(t *testing.T, s *state) {
			if !reflect.DeepEqual(s.events, []string{"deployed"}) {
				t.Fatalf("inference stage did not receive deployed state: %v", s.events)
			}
			s.events = append(s.events, "inference-ran")
		}},
		Stage[*state]{Name: "assert", Run: func(t *testing.T, s *state) {
			want := []string{"deployed", "inference-ran"}
			if !reflect.DeepEqual(s.events, want) {
				t.Fatalf("composed events = %v, want %v", s.events, want)
			}
		}},
	)
}

func TestRunScenariosExecutesRegisteredScenarios(t *testing.T) {
	t.Parallel()

	var got []string
	RunScenarios(t,
		Scenario{Name: "first", Run: func(*testing.T) { got = append(got, "first") }},
		Scenario{Name: "second", Run: func(*testing.T) { got = append(got, "second") }},
	)
	if want := []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scenarios = %v, want %v", got, want)
	}
}
