package testgenflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCoordinatorOwnsStageOrderAndGenerationRetry(t *testing.T) {
	repository := newMemoryRepository()
	run := coordinatorTestRun(t, "coordinator-retry")
	if _, created, err := repository.CreateRun(context.Background(), run); err != nil || !created {
		t.Fatalf("create run = created %t, err %v", created, err)
	}

	workers := &fakeTestGenerationWorkers{
		resolvedCommit: testCommit,
		generation: []GenerationResult{
			{ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"}, PatchDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"}, PatchDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
		maven: []MavenResult{
			{Succeeded: true},
			{Diagnostic: "assertion failed", EvidenceRef: "artifact://test-generation/focused-failed"},
			{Succeeded: true, EvidenceRef: "artifact://test-generation/focused-passed"},
			{Succeeded: true, EvidenceRef: "artifact://test-generation/final-report"},
		},
		prURL: "https://gitlab.example/group/project/-/merge_requests/9",
	}
	coordinator := Coordinator{
		Repository: repository,
		Source:     workers,
		Generation: workers,
		Maven:      workers,
		SCM:        workers,
		Now:        func() time.Time { return run.CreatedAt },
	}

	var final Run
	for i := 0; i < 8; i++ {
		var err error
		final, err = coordinator.Advance(context.Background(), run.RunID)
		if err != nil {
			t.Fatalf("advance %d: %v", i+1, err)
		}
		if final.Status == StatusCompleted || final.Status == StatusFailed || final.Status == StatusOutcomeUnknown {
			break
		}
	}
	if final.Status != StatusCompleted || final.TestsAdded != 1 || final.AttemptCount() != 2 || final.PRURL != workers.prURL || final.EvidenceRef != "artifact://test-generation/final-report" {
		t.Fatalf("coordinated run = %#v", final)
	}
	wantCalls := []string{"source", "maven:package", "generate:1", "maven:focused_tests", "generate:2", "maven:focused_tests", "maven:verify", "scm:open_pr"}
	if fmt.Sprint(workers.calls) != fmt.Sprint(wantCalls) {
		t.Fatalf("worker order = %v, want %v", workers.calls, wantCalls)
	}
	if workers.generations[1].RepairFeedback != "assertion failed" {
		t.Fatalf("repair feedback = %q", workers.generations[1].RepairFeedback)
	}
	if workers.generations[0].Request.ProfileID != run.Request.ProfileID || workers.generations[0].Request.ProfileVersion != run.Request.ProfileVersion || workers.generations[0].Request.ProfileDigest != run.Request.ProfileDigest {
		t.Fatalf("generation worker profile identity = %#v, want configured run profile", workers.generations[0].Request)
	}
	if workers.generations[0].IdempotencyKey == workers.generations[1].IdempotencyKey {
		t.Fatalf("generation attempts reused work key %q", workers.generations[0].IdempotencyKey)
	}
}

func TestCoordinatorStopsWhenGenerationProducesNoTestChanges(t *testing.T) {
	repository := newMemoryRepository()
	run := coordinatorTestRun(t, "coordinator-empty")
	if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	workers := &fakeTestGenerationWorkers{
		resolvedCommit: testCommit,
		generation:     []GenerationResult{{}},
		maven:          []MavenResult{{Succeeded: true}},
	}
	coordinator := Coordinator{Repository: repository, Source: workers, Generation: workers, Maven: workers, SCM: workers, Now: func() time.Time { return run.CreatedAt }}
	var final Run
	for i := 0; i < 4; i++ {
		var err error
		final, err = coordinator.Advance(context.Background(), run.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status == StatusCompleted {
			break
		}
	}
	if final.Status != StatusCompleted || final.TestsAdded != 0 || final.PRURL != "" || final.AttemptCount() != 1 {
		t.Fatalf("empty generation result = %#v", final)
	}
	if fmt.Sprint(workers.calls) != "[source maven:package generate:1]" {
		t.Fatalf("empty generation ran unexpected stages: %v", workers.calls)
	}
}

func TestCoordinatorDoesNotRetryUnknownWorkerOutcome(t *testing.T) {
	repository := newMemoryRepository()
	run := coordinatorTestRun(t, "coordinator-unknown")
	if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	workers := &fakeTestGenerationWorkers{sourceErr: &WorkerFailure{OutcomeUnknown: true, Diagnostic: "submission acknowledgement lost"}}
	coordinator := Coordinator{Repository: repository, Source: workers, Now: func() time.Time { return run.CreatedAt }}
	unknown, err := coordinator.Advance(context.Background(), run.RunID)
	if err != nil || unknown.Status != StatusOutcomeUnknown {
		t.Fatalf("unknown source outcome = %#v, %v", unknown, err)
	}
	again, err := coordinator.Advance(context.Background(), run.RunID)
	if err != nil || again.Status != StatusOutcomeUnknown || len(workers.calls) != 1 {
		t.Fatalf("unknown outcome was retried: run=%#v calls=%v err=%v", again, workers.calls, err)
	}
}

func TestCoordinatorRequiresSignatureWorkerBeforeBaseline(t *testing.T) {
	repository := newMemoryRepository()
	run := coordinatorTestRun(t, "coordinator-signature")
	run.Request.VerifyCommit = true
	requestDigest, err := requestDigest(run.Request, run.MaxAttempts)
	if err != nil {
		t.Fatal(err)
	}
	run.RequestDigest = requestDigest
	if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	workers := &fakeTestGenerationWorkers{resolvedCommit: testCommit}
	coordinator := Coordinator{Repository: repository, Source: workers, Now: func() time.Time { return run.CreatedAt }}
	resolved, err := coordinator.Advance(context.Background(), run.RunID)
	if err != nil || resolved.Stage != StageVerifyCommit {
		t.Fatalf("source resolve = %#v, %v", resolved, err)
	}
	verified, err := coordinator.Advance(context.Background(), run.RunID)
	if err != nil || verified.Status != StatusFailed || verified.Stage != StageVerifyCommit {
		t.Fatalf("unconfigured signature worker = %#v, %v", verified, err)
	}
	if fmt.Sprint(workers.calls) != "[source]" {
		t.Fatalf("Maven ran before signature verification: %v", workers.calls)
	}
}

func coordinatorTestRun(t *testing.T, id string) Run {
	t.Helper()
	run, err := NewRun("test-generation-"+id, "idem-"+id, Request{
		RepositoryURL:  "https://gitlab.example/group/spring-demo.git",
		Revision:       testCommit,
		BaseBranch:     "main",
		IssueIID:       "42",
		Subdirectory:   "source",
		ProfileID:      "spring-java17-maven",
		ProfileVersion: "0.1.0",
		ProfileDigest:  "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}, 3, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return run
}

type fakeTestGenerationWorkers struct {
	resolvedCommit string
	sourceErr      error
	generation     []GenerationResult
	maven          []MavenResult
	prURL          string
	calls          []string
	generations    []WorkRequest
}

func (w *fakeTestGenerationWorkers) ResolveSource(_ context.Context, request WorkRequest) (string, error) {
	w.calls = append(w.calls, "source")
	if w.sourceErr != nil {
		return "", w.sourceErr
	}
	return w.resolvedCommit, nil
}

func (w *fakeTestGenerationWorkers) VerifyCommit(context.Context, WorkRequest) (bool, string, error) {
	w.calls = append(w.calls, "signature")
	return true, "", nil
}

func (w *fakeTestGenerationWorkers) GenerateTests(_ context.Context, request WorkRequest) (GenerationResult, error) {
	w.calls = append(w.calls, fmt.Sprintf("generate:%d", request.Attempt))
	w.generations = append(w.generations, request)
	if len(w.generation) == 0 {
		return GenerationResult{}, errors.New("unscripted generation")
	}
	result := w.generation[0]
	w.generation = w.generation[1:]
	return result, nil
}

func (w *fakeTestGenerationWorkers) RunMaven(_ context.Context, request MavenWorkRequest) (MavenResult, error) {
	w.calls = append(w.calls, "maven:"+string(request.Phase))
	if len(w.maven) == 0 {
		return MavenResult{}, errors.New("unscripted Maven phase")
	}
	result := w.maven[0]
	w.maven = w.maven[1:]
	return result, nil
}

func (w *fakeTestGenerationWorkers) OpenTestsPR(context.Context, WorkRequest) (string, error) {
	w.calls = append(w.calls, "scm:open_pr")
	return w.prURL, nil
}

type memoryRepository struct {
	mu     sync.Mutex
	runs   map[string]Run
	keys   map[string]string
	events map[string]map[string]string
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{runs: make(map[string]Run), keys: make(map[string]string), events: make(map[string]map[string]string)}
}

func (r *memoryRepository) CreateRun(_ context.Context, run Run) (Run, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.keys[run.IdempotencyKey]; ok {
		existing := r.runs[id]
		if existing.RequestDigest != run.RequestDigest {
			return Run{}, false, ErrIdempotencyConflict
		}
		return cloneRun(existing), false, nil
	}
	r.runs[run.RunID] = cloneRun(run)
	r.keys[run.IdempotencyKey] = run.RunID
	r.events[run.RunID] = make(map[string]string)
	return cloneRun(run), true, nil
}

func (r *memoryRepository) GetRun(_ context.Context, runID string) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[runID]
	if !ok {
		return Run{}, ErrRunNotFound
	}
	return cloneRun(run), nil
}

func (r *memoryRepository) ApplyEvent(_ context.Context, runID string, event Event) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[runID]
	if !ok {
		return Run{}, ErrRunNotFound
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return Run{}, err
	}
	digestValue := sha256.Sum256(encoded)
	encodedDigest := "sha256:" + hex.EncodeToString(digestValue[:])
	if prior, ok := r.events[runID][event.ID]; ok {
		if prior != encodedDigest {
			return Run{}, ErrEventConflict
		}
		return cloneRun(run), nil
	}
	next, err := run.Apply(event)
	if err != nil {
		return Run{}, err
	}
	r.runs[runID] = cloneRun(next)
	r.events[runID][event.ID] = encodedDigest
	return cloneRun(next), nil
}

var _ Repository = (*memoryRepository)(nil)
