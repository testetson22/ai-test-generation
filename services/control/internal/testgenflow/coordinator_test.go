package testgenflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
			{ChangedTestPaths: []string{"src/test/java/example/FirstTest.java"}, PatchDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TestArtifactDigest: digest([]byte("first artifact"))},
			{ChangedTestPaths: []string{"src/test/java/example/RepairedTest.java"}, PatchDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", TestArtifactDigest: digest([]byte("repaired artifact"))},
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
	first, repaired, finalVerify := workers.mavenRequests[1], workers.mavenRequests[2], workers.mavenRequests[3]
	if first.IdempotencyKey == repaired.IdempotencyKey || first.Attempt != 1 || repaired.Attempt != 2 || finalVerify.Attempt != 2 {
		t.Fatalf("repair execution identities = %#v, %#v, %#v", first, repaired, finalVerify)
	}
	for _, work := range []WorkRequest{first.WorkRequest, repaired.WorkRequest, finalVerify.WorkRequest} {
		candidate := final.Attempts[work.Attempt-1]
		if !reflect.DeepEqual(work.ChangedTestPaths, candidate.ChangedTestPaths) || work.PatchDigest != candidate.PatchDigest || work.TestArtifactDigest != candidate.TestArtifactDigest {
			t.Fatalf("execution request does not bind its candidate: work=%#v candidate=%#v", work, candidate)
		}
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

type retainedTestFailure struct {
	ref   string
	cause error
}

func (e *retainedTestFailure) Error() string            { return e.cause.Error() }
func (e *retainedTestFailure) Unwrap() error            { return e.cause }
func (e *retainedTestFailure) RetainedEvidence() string { return e.ref }

func TestCoordinatorRetainsWrappedSourceFailureEvidence(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			run := coordinatorTestRun(t, "retained-source-failure")
			repository := newMemoryRepository()
			if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			ref := digest([]byte("retained failed-source diagnostics"))
			workers := &fakeTestGenerationWorkers{sourceErr: fmt.Errorf("source: %w", &retainedTestFailure{ref: ref, cause: &WorkerFailure{Diagnostic: "source failed", OutcomeUnknown: unknown}})}
			coordinator := Coordinator{Repository: repository, Source: workers, Now: func() time.Time { return run.CreatedAt }}
			result, err := coordinator.Advance(context.Background(), run.RunID)
			want := StatusFailed
			if unknown {
				want = StatusOutcomeUnknown
			}
			if err != nil || result.Status != want || result.EvidenceRef != ref {
				t.Fatalf("retained failure: %#v %v", result, err)
			}
		})
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

func TestCoordinatorArtifactInputPublishesVerifiedPatchWithoutGenerationOrSCM(t *testing.T) {
	run := coordinatorRunAtStage(t, StageSource)
	repository := newMemoryRepository()
	if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	candidate := artifactCandidateResult()
	workers := &fakeTestGenerationWorkers{
		resolvedCommit: testCommit,
		prepared:       candidate,
		maven: []MavenResult{
			{Succeeded: true},
			{Succeeded: true, EvidenceRef: "artifact://focused"},
			{Succeeded: true, EvidenceRef: "artifact://verified"},
		},
		resultArtifact: "artifact://published/patch-manifest",
	}
	coordinator := Coordinator{Repository: repository, Source: workers, ArtifactInput: workers, Maven: workers, Publisher: workers, Now: func() time.Time { return run.CreatedAt }}
	for _, stage := range []Stage{StageBaseline, StageArtifactInput, StageFocusedTest, StageFinalVerify, StagePublishPatch, StageDone} {
		var err error
		run, err = coordinator.Advance(context.Background(), run.RunID)
		if err != nil || run.Stage != stage || run.AttemptCount() != 0 || run.PRURL != "" {
			t.Fatalf("advance to %s = %#v, %v", stage, run, err)
		}
		if stage == StageFocusedTest && (run.TestsAdded != 0 || run.PatchDigest != "" || run.TestArtifactDigest != "") {
			t.Fatalf("pending artifact was accepted before focused tests: %#v", run)
		}
		if stage == StagePublishPatch && (run.Status != StatusRunning || run.ResultArtifactRef != "") {
			t.Fatalf("verification completed without publishing: %#v", run)
		}
	}
	if run.Status != StatusCompleted || run.ResultArtifactRef != workers.resultArtifact || run.TestsAdded != 1 || run.Candidate == nil || run.Candidate.Status != AttemptAccepted {
		t.Fatalf("published artifact result = %#v", run)
	}
	wantCalls := []string{"source", "maven:package", "artifact:prepare", "maven:focused_tests", "maven:verify", "patch:publish"}
	if !reflect.DeepEqual(workers.calls, wantCalls) {
		t.Fatalf("artifact workflow calls = %v, want %v", workers.calls, wantCalls)
	}
	for _, work := range []WorkRequest{workers.mavenRequests[1].WorkRequest, workers.mavenRequests[2].WorkRequest, workers.publications[0]} {
		if !reflect.DeepEqual(work.ChangedTestPaths, candidate.ChangedTestPaths) || work.PatchDigest != candidate.PatchDigest || work.TestArtifactDigest != candidate.TestArtifactDigest || work.Attempt != 0 {
			t.Fatalf("artifact execution/publication is not bound to candidate: %#v", work)
		}
	}
	if workers.preparations[0].Request.InputArtifactDigest != run.Request.InputArtifactDigest {
		t.Fatalf("artifact input lost uploaded content address: %#v", workers.preparations[0])
	}
	pipeline, task := run.CompatibilityResults()
	if pipeline.ResultArtifactRef != workers.resultArtifact || pipeline.PRURL != "" || task.Attempts != "0" || pipeline.TerminalStatus != string(StatusCompleted) {
		t.Fatalf("artifact compatibility results = %#v, %#v", pipeline, task)
	}
	again, err := coordinator.Advance(context.Background(), run.RunID)
	if err != nil || !reflect.DeepEqual(again, run) || !reflect.DeepEqual(workers.calls, wantCalls) {
		t.Fatalf("completed run was republished: %#v, calls=%v, err=%v", again, workers.calls, err)
	}
}

func TestCoordinatorPatchPublisherMustSucceedWithNonemptyResult(t *testing.T) {
	for _, test := range []struct {
		name         string
		ref          string
		err          error
		unconfigured bool
	}{
		{name: "failure", err: errors.New("artifact persistence failed")},
		{name: "empty"},
		{name: "whitespace", ref: "   "},
		{name: "unconfigured", unconfigured: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := coordinatorRunAtStage(t, StagePublishPatch)
			repository := newMemoryRepository()
			if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			workers := &fakeTestGenerationWorkers{resultArtifact: test.ref, operationErr: test.err}
			coordinator := Coordinator{Repository: repository, Publisher: workers, Now: func() time.Time { return run.UpdatedAt }}
			if test.unconfigured {
				coordinator.Publisher = nil
			}
			failed, err := coordinator.Advance(context.Background(), run.RunID)
			if err != nil || failed.Status != StatusFailed || failed.Stage != StagePublishPatch || failed.ResultArtifactRef != "" || failed.PRURL != "" || failed.LastError == "" {
				t.Fatalf("invalid publication = %#v, %v", failed, err)
			}
		})
	}
}

func TestCoordinatorArtifactInputRejectsIncompleteCandidates(t *testing.T) {
	for name, mutate := range map[string]func(*GenerationResult){
		"no paths":         func(result *GenerationResult) { result.ChangedTestPaths = nil },
		"no patch":         func(result *GenerationResult) { result.PatchDigest = "" },
		"no artifact":      func(result *GenerationResult) { result.TestArtifactDigest = "" },
		"invalid artifact": func(result *GenerationResult) { result.TestArtifactDigest = "artifact://unbound" },
		"unsafe path":      func(result *GenerationResult) { result.ChangedTestPaths = []string{"pom.xml"} },
	} {
		t.Run(name, func(t *testing.T) {
			run := coordinatorRunAtStage(t, StageArtifactInput)
			repository := newMemoryRepository()
			if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			result := artifactCandidateResult()
			mutate(&result)
			workers := &fakeTestGenerationWorkers{prepared: result}
			coordinator := Coordinator{Repository: repository, ArtifactInput: workers, Now: func() time.Time { return run.UpdatedAt }}
			failed, err := coordinator.Advance(context.Background(), run.RunID)
			if err != nil || failed.Status != StatusFailed || failed.Stage != StageArtifactInput || failed.AttemptCount() != 0 || failed.Candidate != nil || failed.ResultArtifactRef != "" {
				t.Fatalf("incomplete candidate = %#v, %v", failed, err)
			}
		})
	}
}

func TestCoordinatorArtifactFocusedFailureDoesNotAuthorizeGeneration(t *testing.T) {
	run := coordinatorRunAtStage(t, StageFocusedTest)
	repository := newMemoryRepository()
	if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	workers := &fakeTestGenerationWorkers{maven: []MavenResult{{Diagnostic: "uploaded test failed", EvidenceRef: "artifact://focused-failure"}}}
	coordinator := Coordinator{Repository: repository, Maven: workers, Now: func() time.Time { return run.UpdatedAt }}
	failed, err := coordinator.Advance(context.Background(), run.RunID)
	if err != nil || failed.Status != StatusFailed || failed.Stage != StageFocusedTest || failed.AttemptCount() != 0 || failed.Candidate.Status != AttemptFailed || failed.TestsAdded != 0 || failed.ResultArtifactRef != "" {
		t.Fatalf("failed artifact tests = %#v, %v", failed, err)
	}
}

func TestCoordinatorInputAndCompletionModesAreIndependent(t *testing.T) {
	for _, artifactInput := range []bool{false, true} {
		for _, patchOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("artifact=%t/patch=%t", artifactInput, patchOnly), func(t *testing.T) {
				request := newRun(t, false, 3).Request
				if artifactInput {
					request.InputMode = InputModeArtifactInput
					request.InputArtifactDigest = digest([]byte("uploaded artifact"))
				}
				if patchOnly {
					request.CompletionMode = CompletionModePatchOnly
				}
				run, err := NewRun("mode-run", "mode-idem", request, 3, time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatal(err)
				}
				repository := newMemoryRepository()
				if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
					t.Fatal(err)
				}
				workers := &fakeTestGenerationWorkers{resolvedCommit: testCommit, prepared: artifactCandidateResult(), generation: []GenerationResult{artifactCandidateResult()}, resultArtifact: "artifact://patch", prURL: "https://git.example/pr/1", maven: []MavenResult{{Succeeded: true}, {Succeeded: true, EvidenceRef: "artifact://focused"}, {Succeeded: true, EvidenceRef: "artifact://verify"}}}
				coordinator := Coordinator{Repository: repository, Source: workers, Maven: workers, Now: func() time.Time { return run.UpdatedAt }}
				if artifactInput {
					coordinator.ArtifactInput = workers
				} else {
					coordinator.Generation = workers
				}
				if patchOnly {
					coordinator.Publisher = workers
				} else {
					coordinator.SCM = workers
				}
				for step := 0; step < 6; step++ {
					run, err = coordinator.Advance(context.Background(), run.RunID)
					if err != nil {
						t.Fatal(err)
					}
				}
				if run.Status != StatusCompleted || (run.AttemptCount() == 0) != artifactInput || (run.ResultArtifactRef != "") != patchOnly || (run.PRURL == "") != patchOnly {
					t.Fatalf("mode-specific ports/results = %#v, calls=%v", run, workers.calls)
				}
			})
		}
	}
}

func TestCoordinatorPendingAndCanceledWorkNeverAppendsEvents(t *testing.T) {
	for _, stage := range []Stage{StageSource, StageVerifyCommit, StageBaseline, StageGenerate, StageArtifactInput, StageFocusedTest, StageFinalVerify, StageOpenPR, StagePublishPatch} {
		for _, behavior := range []string{"pending", "wrapped pending", "canceled before call", "canceled during success", "canceled during failure", "returned cancellation", "returned deadline"} {
			t.Run(string(stage)+"/"+behavior, func(t *testing.T) {
				run := coordinatorRunAtStage(t, stage)
				repository := newMemoryRepository()
				if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				workers := &fakeTestGenerationWorkers{resolvedCommit: testCommit, prepared: artifactCandidateResult(), generation: []GenerationResult{artifactCandidateResult()}, resultArtifact: "artifact://patch", prURL: "https://git.example/pr/1", maven: []MavenResult{{Succeeded: true, EvidenceRef: "artifact://report"}}}
				wantErr := ErrWorkPending
				switch behavior {
				case "pending":
					workers.operationErr = fmt.Errorf("adapter: %w", ErrWorkPending)
				case "wrapped pending":
					workers.operationErr = errors.Join(ErrWorkPending, &WorkerFailure{OutcomeUnknown: true, Diagnostic: "still running", Cause: errors.New("observation unavailable")})
				case "canceled before call":
					cancel()
					wantErr = context.Canceled
				case "canceled during success":
					workers.cancel = cancel
					wantErr = context.Canceled
				case "canceled during failure":
					workers.cancel = cancel
					workers.operationErr = &WorkerFailure{Diagnostic: "worker stopped", Cause: context.Canceled}
					wantErr = context.Canceled
				case "returned cancellation":
					workers.operationErr = fmt.Errorf("adapter: %w", context.Canceled)
					wantErr = context.Canceled
				case "returned deadline":
					workers.operationErr = fmt.Errorf("adapter: %w", context.DeadlineExceeded)
					wantErr = context.DeadlineExceeded
				}
				coordinator := Coordinator{Repository: repository, Source: workers, Signature: workers, Generation: workers, ArtifactInput: workers, Maven: workers, SCM: workers, Publisher: workers, Now: func() time.Time { return run.UpdatedAt }}
				unchanged, err := coordinator.Advance(ctx, run.RunID)
				if !errors.Is(err, wantErr) {
					t.Fatalf("advance error = %v, want %v", err, wantErr)
				}
				stored, err := repository.GetRun(context.Background(), run.RunID)
				if err != nil || !reflect.DeepEqual(stored, run) || len(repository.events[run.RunID]) != 0 {
					t.Fatalf("pending/canceled work mutated state: before=%#v after=%#v events=%v err=%v", run, stored, repository.events, err)
				}
				if behavior != "canceled before call" && !reflect.DeepEqual(unchanged, run) {
					t.Fatalf("pending/canceled return changed run: %#v", unchanged)
				}
				if behavior == "canceled before call" && len(workers.calls) != 0 {
					t.Fatalf("pre-canceled context invoked workers: %v", workers.calls)
				}
			})
		}
	}
}

func TestCoordinatorReconcilesExistingGenerationAttemptWithoutNewStartEvent(t *testing.T) {
	run := coordinatorRunAtStage(t, StageGenerate)
	run = apply(t, run, Event{Type: EventGenerationStarted}, run.UpdatedAt.Add(time.Second))
	repository := newMemoryRepository()
	if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	workers := &fakeTestGenerationWorkers{operationErr: ErrWorkPending}
	coordinator := Coordinator{Repository: repository, Generation: workers, Now: func() time.Time { return run.UpdatedAt }}
	unchanged, err := coordinator.Advance(context.Background(), run.RunID)
	if !errors.Is(err, ErrWorkPending) || !reflect.DeepEqual(unchanged, run) || len(repository.events[run.RunID]) != 0 || !workers.generations[0].ReconcileOnly || workers.generations[0].Attempt != 1 {
		t.Fatalf("pending reconciliation = %#v, work=%#v, err=%v", unchanged, workers.generations, err)
	}
}

func artifactCandidateResult() GenerationResult {
	return GenerationResult{ChangedTestPaths: []string{"src/test/java/example/UploadedTest.java"}, PatchDigest: digest([]byte("candidate patch")), TestArtifactDigest: digest([]byte("retrievable test artifact"))}
}

func coordinatorRunAtStage(t *testing.T, stage Stage) Run {
	t.Helper()
	base := newRun(t, stage == StageVerifyCommit, 3)
	request := base.Request
	if stage != StageGenerate {
		request.InputMode = InputModeArtifactInput
		request.InputArtifactDigest = digest([]byte("uploaded artifact"))
	}
	if stage != StageOpenPR {
		request.CompletionMode = CompletionModePatchOnly
	}
	run, err := NewRun(base.RunID, base.IdempotencyKey, request, base.MaxAttempts, base.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for run.Stage != stage {
		var event Event
		switch run.Stage {
		case StageSource:
			event = Event{Type: EventSourceResolved, ResolvedCommit: testCommit}
		case StageVerifyCommit:
			event = Event{Type: EventSignatureVerified, Succeeded: true}
		case StageBaseline:
			event = Event{Type: EventBaselineCompleted, Succeeded: true}
		case StageArtifactInput:
			candidate := artifactCandidateResult()
			event = Event{Type: EventArtifactInputCompleted, ChangedTestPaths: candidate.ChangedTestPaths, PatchDigest: candidate.PatchDigest, TestArtifactDigest: candidate.TestArtifactDigest}
		case StageFocusedTest:
			event = Event{Type: EventFocusedTestCompleted, Succeeded: true, EvidenceRef: "artifact://focused"}
		case StageFinalVerify:
			event = Event{Type: EventFinalVerifyCompleted, Succeeded: true, EvidenceRef: "artifact://verify"}
		default:
			t.Fatalf("cannot prepare stage %s from %s", stage, run.Stage)
		}
		run = apply(t, run, event, run.UpdatedAt.Add(time.Second))
	}
	return run
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
	prepared       GenerationResult
	resultArtifact string
	operationErr   error
	cancel         context.CancelFunc
	mavenRequests  []MavenWorkRequest
	preparations   []WorkRequest
	publications   []WorkRequest
}

func (w *fakeTestGenerationWorkers) ResolveSource(_ context.Context, request WorkRequest) (string, error) {
	w.calls = append(w.calls, "source")
	if w.cancel != nil {
		w.cancel()
	}
	if w.operationErr != nil {
		return "", w.operationErr
	}
	if w.sourceErr != nil {
		return "", w.sourceErr
	}
	return w.resolvedCommit, nil
}

func (w *fakeTestGenerationWorkers) VerifyCommit(context.Context, WorkRequest) (bool, string, error) {
	w.calls = append(w.calls, "signature")
	if w.cancel != nil {
		w.cancel()
	}
	if w.operationErr != nil {
		return false, "", w.operationErr
	}
	return true, "", nil
}

func (w *fakeTestGenerationWorkers) GenerateTests(_ context.Context, request WorkRequest) (GenerationResult, error) {
	w.calls = append(w.calls, fmt.Sprintf("generate:%d", request.Attempt))
	w.generations = append(w.generations, request)
	if w.cancel != nil {
		w.cancel()
	}
	if w.operationErr != nil {
		return GenerationResult{}, w.operationErr
	}
	if len(w.generation) == 0 {
		return GenerationResult{}, errors.New("unscripted generation")
	}
	result := w.generation[0]
	w.generation = w.generation[1:]
	return result, nil
}

func (w *fakeTestGenerationWorkers) RunMaven(_ context.Context, request MavenWorkRequest) (MavenResult, error) {
	w.calls = append(w.calls, "maven:"+string(request.Phase))
	w.mavenRequests = append(w.mavenRequests, request)
	if w.cancel != nil {
		w.cancel()
	}
	if w.operationErr != nil {
		return MavenResult{}, w.operationErr
	}
	if len(w.maven) == 0 {
		return MavenResult{}, errors.New("unscripted Maven phase")
	}
	result := w.maven[0]
	w.maven = w.maven[1:]
	return result, nil
}

func (w *fakeTestGenerationWorkers) OpenTestsPR(context.Context, WorkRequest) (string, error) {
	w.calls = append(w.calls, "scm:open_pr")
	if w.cancel != nil {
		w.cancel()
	}
	if w.operationErr != nil {
		return "", w.operationErr
	}
	return w.prURL, nil
}

func (w *fakeTestGenerationWorkers) PrepareTestArtifact(_ context.Context, request WorkRequest) (GenerationResult, error) {
	w.calls = append(w.calls, "artifact:prepare")
	w.preparations = append(w.preparations, request)
	if w.cancel != nil {
		w.cancel()
	}
	return w.prepared, w.operationErr
}

func (w *fakeTestGenerationWorkers) PublishPatch(_ context.Context, request WorkRequest) (string, error) {
	w.calls = append(w.calls, "patch:publish")
	w.publications = append(w.publications, request)
	if w.cancel != nil {
		w.cancel()
	}
	return w.resultArtifact, w.operationErr
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
