package testkit

import (
	"errors"
	"sync"
)

type WorkerPhase string

const (
	PhasePrepare  WorkerPhase = "prepare"
	PhaseBuild    WorkerPhase = "build"
	PhaseDiscover WorkerPhase = "discover"
	PhaseTest     WorkerPhase = "test"
	PhaseCollect  WorkerPhase = "collect"
	PhaseCleanup  WorkerPhase = "cleanup"
)

var ErrUnscriptedPhase = errors.New("worker phase has no scripted result")

type PhaseResult struct {
	Status          string
	FailureCategory string
	ExecutedTestIDs []string
	ArtifactDigests []string
}

// ScriptedWorker returns explicit phase results. An absent script is an error,
// never an implicit successful phase.
type ScriptedWorker struct {
	mu      sync.Mutex
	results map[WorkerPhase][]PhaseResult
	calls   []WorkerPhase
}

func NewScriptedWorker(results map[WorkerPhase][]PhaseResult) *ScriptedWorker {
	copyOfResults := make(map[WorkerPhase][]PhaseResult, len(results))
	for phase, phaseResults := range results {
		copyOfResults[phase] = make([]PhaseResult, len(phaseResults))
		for i, result := range phaseResults {
			copyOfResults[phase][i] = clonePhaseResult(result)
		}
	}
	return &ScriptedWorker{results: copyOfResults}
}

func (w *ScriptedWorker) RunPhase(phase WorkerPhase) (PhaseResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, phase)
	results := w.results[phase]
	if len(results) == 0 {
		return PhaseResult{}, ErrUnscriptedPhase
	}
	result := results[0]
	w.results[phase] = results[1:]
	return clonePhaseResult(result), nil
}

func (w *ScriptedWorker) Calls() []WorkerPhase {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]WorkerPhase(nil), w.calls...)
}

func clonePhaseResult(result PhaseResult) PhaseResult {
	result.ExecutedTestIDs = append([]string(nil), result.ExecutedTestIDs...)
	result.ArtifactDigests = append([]string(nil), result.ArtifactDigests...)
	return result
}
