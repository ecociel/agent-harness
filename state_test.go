package harness_test

import (
	"testing"

	"github.com/ecociel/agent-harness"
)

func TestTransitionTable(t *testing.T) {
	tests := []struct {
		from, to harness.State
		ok       bool
	}{
		{harness.StateStart, harness.StatePrepare, true},
		{harness.StateStart, harness.StateModel, false},
		{harness.StatePrepare, harness.StateCompose, true},
		{harness.StatePrepare, harness.StateStopped, true},
		{harness.StateCompose, harness.StateModel, true},
		{harness.StateCompose, harness.StateTools, false},
		{harness.StateModel, harness.StateRoute, true},
		{harness.StateModel, harness.StateVerify, false},
		{harness.StateRoute, harness.StateTools, true},
		{harness.StateRoute, harness.StateVerify, true},
		{harness.StateRoute, harness.StateCompose, false},
		{harness.StateTools, harness.StateObserve, true},
		{harness.StateTools, harness.StateCompose, false},
		{harness.StateObserve, harness.StateCompose, true},
		{harness.StateVerify, harness.StateDone, true},
		{harness.StateVerify, harness.StateCompose, true},
		{harness.StateVerify, harness.StateTools, false},
		{harness.StateDone, harness.StateCompose, false},
		{harness.StateStopped, harness.StateStart, false},
		{harness.StateFailed, harness.StateDone, false},
	}
	for _, tt := range tests {
		if got := harness.CanTransition(tt.from, tt.to); got != tt.ok {
			t.Errorf("%s → %s = %v, want %v", tt.from, tt.to, got, tt.ok)
		}
	}
}

func TestEveryLiveStateCanStopAndFail(t *testing.T) {
	for _, s := range harness.States() {
		if s.Terminal() {
			if len(harness.Transitions()[s]) != 0 {
				t.Errorf("terminal %s has outgoing edges", s)
			}
			continue
		}
		if !harness.CanTransition(s, harness.StateStopped) || !harness.CanTransition(s, harness.StateFailed) {
			t.Errorf("%s cannot stop or fail", s)
		}
	}
}

func TestLoopClassification(t *testing.T) {
	tests := map[harness.State]string{
		harness.StateStart: "outer", harness.StatePrepare: "outer", harness.StateVerify: "outer",
		harness.StateCompose: "inner", harness.StateModel: "inner", harness.StateRoute: "inner",
		harness.StateTools: "inner", harness.StateObserve: "inner",
		harness.StateDone: "terminal", harness.StateStopped: "terminal", harness.StateFailed: "terminal",
	}
	for s, want := range tests {
		if got := s.Loop(); got != want {
			t.Errorf("%s.Loop() = %s, want %s", s, got, want)
		}
	}
}
