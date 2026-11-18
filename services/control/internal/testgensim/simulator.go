package testgensim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

//go:embed offline-profile.json
var offlineProfileBytes []byte

type Scenario string

const (
	ScenarioSuccess       Scenario = "success"
	ScenarioRetry         Scenario = "retry"
	ScenarioNoChanges     Scenario = "no-changes"
	ScenarioUnknownResult Scenario = "unknown-outcome"
)

type ProfileIdentity struct {
	ID              string `json:"profile_id"`
	Version         string `json:"profile_version"`
	Digest          string `json:"profile_digest"`
	Mode            string `json:"mode"`
	Description     string `json:"description"`
	ExternalEffects bool   `json:"external_effects"`
}

type Report struct {
	SimulationOnly        bool                              `json:"simulation_only"`
	SafetyNote            string                            `json:"safety_note"`
	Scenario              Scenario                          `json:"scenario"`
	Profile               ProfileIdentity                   `json:"profile"`
	WorkerCalls           []string                          `json:"worker_calls"`
	Run                   testgenflow.Run                   `json:"run"`
	PipelineResults       testgenflow.PipelineResults       `json:"pipeline_results"`
	GenerationTaskResults testgenflow.GenerationTaskResults `json:"generation_task_results"`
}

const safetyNote = "Simulation only: no repository checkout, network request, subprocess, Maven command, or cluster operation is performed."

func Run(ctx context.Context, scenario Scenario) (Report, error) {
	if !validScenario(scenario) {
		return Report{}, fmt.Errorf("unsupported simulation scenario %q", scenario)
	}
	profile, err := loadProfileIdentity()
	if err != nil {
		return Report{}, err
	}
	request := testgenflow.Request{
		RepositoryURL:  "https://example.invalid/sample.git",
		Revision:       strings.Repeat("a", 40),
		BaseBranch:     "main",
		Subdirectory:   "source",
		ProfileID:      profile.ID,
		ProfileVersion: profile.Version,
		ProfileDigest:  profile.Digest,
	}
	runID := "offline-simulation-" + string(scenario)
	run, err := testgenflow.NewRun(runID, "offline-simulation:"+string(scenario), request, testgenflow.DefaultMaxGenerationAttempts, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return Report{}, fmt.Errorf("create simulated run: %w", err)
	}
	repository := newMemoryRepository()
	if _, _, err := repository.CreateRun(ctx, run); err != nil {
		return Report{}, fmt.Errorf("store simulated run: %w", err)
	}
	workers := &simulatedWorkers{scenario: scenario, revision: request.Revision}
	clock := run.CreatedAt
	coordinator := testgenflow.Coordinator{
		Repository: repository,
		Source:     workers,
		Signature:  workers,
		Generation: workers,
		Maven:      workers,
		SCM:        workers,
		Now: func() time.Time {
			clock = clock.Add(time.Millisecond)
			return clock
		},
	}
	for step := 0; step < 16; step++ {
		run, err = coordinator.Advance(ctx, runID)
		if err != nil {
			return Report{}, fmt.Errorf("advance simulated run: %w", err)
		}
		if terminal(run.Status) {
			pipelineResults, generationResults := run.CompatibilityResults()
			return Report{
				SimulationOnly:        true,
				SafetyNote:            safetyNote,
				Scenario:              scenario,
				Profile:               profile,
				WorkerCalls:           append([]string(nil), workers.calls...),
				Run:                   run,
				PipelineResults:       pipelineResults,
				GenerationTaskResults: generationResults,
			}, nil
		}
	}
	return Report{}, errors.New("simulated flow did not reach a terminal state within 16 coordinator advances")
}

func loadProfileIdentity() (ProfileIdentity, error) {
	var profile ProfileIdentity
	if err := json.Unmarshal(offlineProfileBytes, &profile); err != nil {
		return ProfileIdentity{}, fmt.Errorf("decode embedded simulator profile: %w", err)
	}
	if profile.ID == "" || profile.Version == "" || profile.Mode != "simulation-only" || profile.Description == "" || profile.ExternalEffects {
		return ProfileIdentity{}, errors.New("embedded profile is not restricted to offline simulation")
	}
	sum := sha256.Sum256(offlineProfileBytes)
	profile.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return profile, nil
}

func validScenario(scenario Scenario) bool {
	switch scenario {
	case ScenarioSuccess, ScenarioRetry, ScenarioNoChanges, ScenarioUnknownResult:
		return true
	default:
		return false
	}
}

func terminal(status testgenflow.Status) bool {
	switch status {
	case testgenflow.StatusCompleted, testgenflow.StatusFailed, testgenflow.StatusCancelled, testgenflow.StatusOutcomeUnknown:
		return true
	default:
		return false
	}
}

type simulatedWorkers struct {
	scenario Scenario
	revision string
	calls    []string
}

func (w *simulatedWorkers) ResolveSource(ctx context.Context, _ testgenflow.WorkRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	w.calls = append(w.calls, "source:resolve")
	if w.scenario == ScenarioUnknownResult {
		return "", &testgenflow.WorkerFailure{OutcomeUnknown: true, Diagnostic: "simulated submission acknowledgement lost"}
	}
	return w.revision, nil
}

func (w *simulatedWorkers) VerifyCommit(ctx context.Context, _ testgenflow.WorkRequest) (bool, string, error) {
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	w.calls = append(w.calls, "source:verify-signature")
	return true, "", nil
}

func (w *simulatedWorkers) GenerateTests(ctx context.Context, request testgenflow.WorkRequest) (testgenflow.GenerationResult, error) {
	if err := ctx.Err(); err != nil {
		return testgenflow.GenerationResult{}, err
	}
	w.calls = append(w.calls, fmt.Sprintf("generation:attempt-%d", request.Attempt))
	if w.scenario == ScenarioNoChanges {
		return testgenflow.GenerationResult{}, nil
	}
	patch := sha256.Sum256([]byte("offline-simulator-generated-test-v1"))
	return testgenflow.GenerationResult{
		ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"},
		PatchDigest:      "sha256:" + hex.EncodeToString(patch[:]),
	}, nil
}

func (w *simulatedWorkers) RunMaven(ctx context.Context, request testgenflow.MavenWorkRequest) (testgenflow.MavenResult, error) {
	if err := ctx.Err(); err != nil {
		return testgenflow.MavenResult{}, err
	}
	w.calls = append(w.calls, "maven:"+string(request.Phase))
	switch request.Phase {
	case testgenflow.MavenPhasePackage:
		return testgenflow.MavenResult{Succeeded: true}, nil
	case testgenflow.MavenPhaseFocusedTests:
		if w.scenario == ScenarioRetry && request.Attempt == 1 {
			return testgenflow.MavenResult{Diagnostic: "simulated assertion mismatch"}, nil
		}
		return testgenflow.MavenResult{
			Succeeded:   true,
			EvidenceRef: fmt.Sprintf("artifact://offline-simulator/focused-attempt-%d", request.Attempt),
		}, nil
	case testgenflow.MavenPhaseFinalVerify:
		return testgenflow.MavenResult{Succeeded: true, EvidenceRef: "artifact://offline-simulator/final-verify"}, nil
	default:
		return testgenflow.MavenResult{}, fmt.Errorf("unsupported simulated Maven phase %q", request.Phase)
	}
}

func (w *simulatedWorkers) OpenTestsPR(ctx context.Context, _ testgenflow.WorkRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	w.calls = append(w.calls, "scm:simulate-pr")
	return "https://example.invalid/simulated-change/1", nil
}

type memoryRepository struct {
	mu       sync.Mutex
	runs     map[string]testgenflow.Run
	keys     map[string]string
	eventIDs map[string]map[string]string
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{
		runs:     make(map[string]testgenflow.Run),
		keys:     make(map[string]string),
		eventIDs: make(map[string]map[string]string),
	}
}

func (r *memoryRepository) CreateRun(ctx context.Context, run testgenflow.Run) (testgenflow.Run, bool, error) {
	if err := ctx.Err(); err != nil {
		return testgenflow.Run{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if runID, exists := r.keys[run.IdempotencyKey]; exists {
		existing := r.runs[runID]
		if existing.RequestDigest != run.RequestDigest {
			return testgenflow.Run{}, false, testgenflow.ErrIdempotencyConflict
		}
		return cloneRun(existing), false, nil
	}
	r.runs[run.RunID] = cloneRun(run)
	r.keys[run.IdempotencyKey] = run.RunID
	r.eventIDs[run.RunID] = make(map[string]string)
	return cloneRun(run), true, nil
}

func (r *memoryRepository) GetRun(ctx context.Context, runID string) (testgenflow.Run, error) {
	if err := ctx.Err(); err != nil {
		return testgenflow.Run{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, exists := r.runs[runID]
	if !exists {
		return testgenflow.Run{}, testgenflow.ErrRunNotFound
	}
	return cloneRun(run), nil
}

func (r *memoryRepository) ApplyEvent(ctx context.Context, runID string, event testgenflow.Event) (testgenflow.Run, error) {
	if err := ctx.Err(); err != nil {
		return testgenflow.Run{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, exists := r.runs[runID]
	if !exists {
		return testgenflow.Run{}, testgenflow.ErrRunNotFound
	}
	eventBytes, err := json.Marshal(event)
	if err != nil {
		return testgenflow.Run{}, err
	}
	sum := sha256.Sum256(eventBytes)
	eventDigest := hex.EncodeToString(sum[:])
	if prior, exists := r.eventIDs[runID][event.ID]; exists {
		if prior != eventDigest {
			return testgenflow.Run{}, testgenflow.ErrEventConflict
		}
		return cloneRun(run), nil
	}
	next, err := run.Apply(event)
	if err != nil {
		return testgenflow.Run{}, err
	}
	r.runs[runID] = cloneRun(next)
	r.eventIDs[runID][event.ID] = eventDigest
	return cloneRun(next), nil
}

func cloneRun(run testgenflow.Run) testgenflow.Run {
	run.Attempts = append([]testgenflow.GenerationAttempt{}, run.Attempts...)
	for i := range run.Attempts {
		run.Attempts[i].ChangedTestPaths = append([]string{}, run.Attempts[i].ChangedTestPaths...)
		if run.Attempts[i].FinishedAt != nil {
			finishedAt := *run.Attempts[i].FinishedAt
			run.Attempts[i].FinishedAt = &finishedAt
		}
	}
	run.ChangedTestPaths = append([]string{}, run.ChangedTestPaths...)
	if run.CancelRequestedAt != nil {
		cancelRequestedAt := *run.CancelRequestedAt
		run.CancelRequestedAt = &cancelRequestedAt
	}
	return run
}

var (
	_ testgenflow.Repository         = (*memoryRepository)(nil)
	_ testgenflow.SourceProvider     = (*simulatedWorkers)(nil)
	_ testgenflow.SignatureVerifier  = (*simulatedWorkers)(nil)
	_ testgenflow.GenerationWorker   = (*simulatedWorkers)(nil)
	_ testgenflow.MavenProfileRunner = (*simulatedWorkers)(nil)
	_ testgenflow.SCMWorker          = (*simulatedWorkers)(nil)
)
