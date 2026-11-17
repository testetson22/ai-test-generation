package testkit

import (
	"errors"
	"reflect"
	"testing"
)

func TestScriptedWorkerRequiresExplicitPhaseResults(t *testing.T) {
	worker := NewScriptedWorker(map[WorkerPhase][]PhaseResult{
		PhaseBuild: {{Status: "failed", FailureCategory: "build_failed"}},
	})
	if _, err := worker.RunPhase(PhasePrepare); !errors.Is(err, ErrUnscriptedPhase) {
		t.Fatalf("unscripted prepare error = %v, want explicit missing script", err)
	}
	result, err := worker.RunPhase(PhaseBuild)
	if err != nil || result.Status != "failed" || result.FailureCategory != "build_failed" {
		t.Fatalf("build result = %#v, error = %v", result, err)
	}
	if got, want := worker.Calls(), []WorkerPhase{PhasePrepare, PhaseBuild}; !reflect.DeepEqual(got, want) {
		t.Fatalf("phase calls = %v, want %v", got, want)
	}
}

func TestScriptedWorkerCopiesInputsAndResults(t *testing.T) {
	ids := []string{"FixtureTest#works"}
	worker := NewScriptedWorker(map[WorkerPhase][]PhaseResult{
		PhaseTest: {{Status: "completed", ExecutedTestIDs: ids}},
	})
	ids[0] = "mutated"
	result, err := worker.RunPhase(PhaseTest)
	if err != nil {
		t.Fatalf("run test phase: %v", err)
	}
	if result.ExecutedTestIDs[0] != "FixtureTest#works" {
		t.Fatalf("worker retained caller mutation: %v", result.ExecutedTestIDs)
	}
	result.ExecutedTestIDs[0] = "changed result"
	if _, err := worker.RunPhase(PhaseTest); !errors.Is(err, ErrUnscriptedPhase) {
		t.Fatalf("consumed phase error = %v, want unscripted", err)
	}
}
