package testgenflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestSuccessfulUnitGenerationFlowPreservesAcceptedTestsAndResults(t *testing.T) {
	run := newRun(t, false, 6)
	now := run.CreatedAt
	run = apply(t, run, Event{Type: EventSourceResolved, ResolvedCommit: testCommit}, now.Add(time.Second))
	if run.Stage != StageBaseline || run.Status != StatusRunning {
		t.Fatalf("source resolution = %s/%s, want running baseline", run.Status, run.Stage)
	}
	run = apply(t, run, Event{Type: EventBaselineCompleted, Succeeded: true}, now.Add(2*time.Second))
	run = apply(t, run, Event{Type: EventGenerationStarted}, now.Add(3*time.Second))
	run = apply(t, run, Event{
		Type:             EventGenerationCompleted,
		ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"},
		PatchDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, now.Add(4*time.Second))
	if run.Stage != StageFocusedTest || run.TestsAdded != 0 {
		t.Fatalf("unverified generated candidate = %s, tests=%d", run.Stage, run.TestsAdded)
	}
	run = apply(t, run, Event{
		Type:        EventFocusedTestCompleted,
		Succeeded:   true,
		EvidenceRef: "artifact://testgen-run/focused-report",
	}, now.Add(5*time.Second))
	run = apply(t, run, Event{
		Type:        EventFinalVerifyCompleted,
		Succeeded:   true,
		EvidenceRef: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}, now.Add(6*time.Second))
	run = apply(t, run, Event{
		Type:      EventPRCompleted,
		Succeeded: true,
		PRURL:     "https://gitlab.example/group/project/-/merge_requests/42",
	}, now.Add(7*time.Second))

	if run.Status != StatusCompleted || run.Stage != StageDone || run.AttemptCount() != 1 || run.TestsAdded != 1 || run.PRURL == "" {
		t.Fatalf("completed run = %#v", run)
	}
	if run.ResolvedCommit != testCommit || run.ChangedTestPaths[0] != "src/test/java/example/GeneratedTest.java" {
		t.Fatalf("accepted source/test identity = %#v %#v", run.ResolvedCommit, run.ChangedTestPaths)
	}
	if err := run.Validate(); err != nil {
		t.Fatalf("completed run validation: %v", err)
	}
}

func TestSignatureVerificationIsOptionalButPrecedesBuildWhenRequested(t *testing.T) {
	run := newRun(t, true, 6)
	now := run.CreatedAt
	run = apply(t, run, Event{Type: EventSourceResolved, ResolvedCommit: testCommit}, now.Add(time.Second))
	if run.Stage != StageVerifyCommit {
		t.Fatalf("stage after source resolution = %q, want signature verification", run.Stage)
	}
	run = apply(t, run, Event{Type: EventSignatureVerified, Succeeded: true}, now.Add(2*time.Second))
	if run.Stage != StageBaseline {
		t.Fatalf("stage after signature verification = %q, want baseline", run.Stage)
	}
	if _, err := run.Apply(Event{ID: "bad-order", ExpectedRevision: run.Revision, Type: EventGenerationStarted, OccurredAt: now.Add(3 * time.Second)}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("generation before baseline error = %v, want transition error", err)
	}
}

func TestRunRequiresImmutableMavenProfileIdentity(t *testing.T) {
	request := newRun(t, false, 6).Request
	for name, mutate := range map[string]func(*Request){
		"missing ID":      func(request *Request) { request.ProfileID = "" },
		"invalid version": func(request *Request) { request.ProfileVersion = "latest" },
		"missing digest":  func(request *Request) { request.ProfileDigest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := request
			mutate(&invalid)
			if err := invalid.Validate(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("invalid profile identity error = %v, want %v", err, ErrInvalidRequest)
			}
		})
	}
}

func TestZeroGeneratedTestsCompletesWithoutVerifyOrPR(t *testing.T) {
	run := startGeneration(t, newRun(t, false, 6))
	run = apply(t, run, Event{Type: EventGenerationCompleted}, run.UpdatedAt.Add(time.Second))
	if run.Status != StatusCompleted || run.Stage != StageDone || run.TestsAdded != 0 || run.PRURL != "" || run.PatchDigest != "" {
		t.Fatalf("no-test result = %#v", run)
	}
	if _, err := run.Apply(Event{ID: "unexpected-pr", ExpectedRevision: run.Revision, Type: EventPRCompleted, Succeeded: true, OccurredAt: run.UpdatedAt.Add(time.Second)}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("PR after zero tests error = %v, want transition error", err)
	}
}

func TestFocusedTestFailuresAuthorizeOnlyBoundedGenerationRetries(t *testing.T) {
	run := newRun(t, false, 2)
	now := run.CreatedAt
	run = startGeneration(t, run)
	run = generateCandidate(t, run, now.Add(3*time.Second))
	run = apply(t, run, Event{Type: EventFocusedTestCompleted, Diagnostic: "assertion failed"}, now.Add(4*time.Second))
	if run.Stage != StageGenerate || run.Status != StatusRunning || run.AttemptCount() != 1 {
		t.Fatalf("first focused-test failure = %s/%s attempts=%d", run.Status, run.Stage, run.AttemptCount())
	}
	run = apply(t, run, Event{Type: EventGenerationStarted}, now.Add(5*time.Second))
	run = generateCandidate(t, run, now.Add(6*time.Second))
	run = apply(t, run, Event{Type: EventFocusedTestCompleted, Diagnostic: "second failure"}, now.Add(7*time.Second))
	if run.Status != StatusFailed || run.AttemptCount() != 2 || run.LastError != "second failure" {
		t.Fatalf("exhausted retries = %#v", run)
	}
	if _, err := run.Apply(Event{ID: "third-attempt", ExpectedRevision: run.Revision, Type: EventGenerationStarted, OccurredAt: now.Add(8 * time.Second)}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("retry after exhaustion error = %v, want transition error", err)
	}
}

func TestFinalVerifyFailureDoesNotCreatePROrRetryGeneration(t *testing.T) {
	run := startGeneration(t, newRun(t, false, 6))
	run = generateCandidate(t, run, run.UpdatedAt.Add(time.Second))
	run = apply(t, run, Event{
		Type:        EventFocusedTestCompleted,
		Succeeded:   true,
		EvidenceRef: "artifact://testgen-run/focused-report",
	}, run.UpdatedAt.Add(time.Second))
	run = apply(t, run, Event{Type: EventFinalVerifyCompleted, Diagnostic: "full suite failed"}, run.UpdatedAt.Add(time.Second))
	if run.Status != StatusFailed || run.Stage != StageFinalVerify || run.AttemptCount() != 1 || run.PRURL != "" {
		t.Fatalf("final verify failure = %#v", run)
	}
	if _, err := run.Apply(Event{ID: "no-pr", ExpectedRevision: run.Revision, Type: EventPRCompleted, Succeeded: true, OccurredAt: run.UpdatedAt.Add(time.Second)}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("PR after failed final verify error = %v, want transition error", err)
	}
}

func TestCommitMismatchAndUnsafeTestPathsAreRejected(t *testing.T) {
	run := newRun(t, false, 6)
	if _, err := run.Apply(Event{ID: "wrong-source", ExpectedRevision: run.Revision, Type: EventSourceResolved, ResolvedCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", OccurredAt: run.CreatedAt.Add(time.Second)}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("wrong source error = %v, want transition error", err)
	}
	run = startGeneration(t, run)
	for _, invalid := range []string{"pom.xml", "src/main/java/example/BadTest.java", "src/test/java/../main/BadTest.java", "/src/test/java/AbsoluteTest.java"} {
		t.Run(invalid, func(t *testing.T) {
			_, err := run.Apply(Event{
				ID:               "unsafe-" + string(rune('a'+len(invalid)%20)),
				ExpectedRevision: run.Revision,
				Type:             EventGenerationCompleted,
				ChangedTestPaths: []string{invalid},
				PatchDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				OccurredAt:       run.UpdatedAt.Add(time.Second),
			})
			if err == nil {
				t.Fatalf("path %q unexpectedly accepted", invalid)
			}
		})
	}
}

func TestCancellationRequiresConfirmedTermination(t *testing.T) {
	run := newRun(t, false, 6)
	now := run.CreatedAt
	run = apply(t, run, Event{Type: EventCancelRequested}, now.Add(time.Second))
	if run.Status != StatusCancelRequested {
		t.Fatalf("cancel request status = %q", run.Status)
	}
	if _, err := run.Apply(Event{ID: "unconfirmed", ExpectedRevision: run.Revision, Type: EventCancelled, OccurredAt: now.Add(2 * time.Second)}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unconfirmed cancellation error = %v, want transition error", err)
	}
	run = apply(t, run, Event{Type: EventCancelled, TerminationConfirmed: true}, now.Add(2*time.Second))
	if run.Status != StatusCancelled {
		t.Fatalf("confirmed cancellation status = %q", run.Status)
	}
}

func TestCancellationRetainsOptionalEvidenceWithoutInventingPublication(t *testing.T) {
	run := coordinatorRunAtStage(t, StagePublishPatch)
	run = apply(t, run, Event{Type: EventCancelRequested}, run.UpdatedAt.Add(time.Second))
	for _, invalid := range []string{" ", "artifact://partial\nunsafe", strings.Repeat("x", 2049)} {
		if _, err := run.Apply(Event{ID: "invalid-cancel-evidence", ExpectedRevision: run.Revision, Type: EventCancelled, TerminationConfirmed: true, EvidenceRef: invalid, OccurredAt: run.UpdatedAt.Add(time.Second)}); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("invalid cancellation evidence was accepted: %v", err)
		}
	}
	cancelled := apply(t, run, Event{Type: EventCancelled, TerminationConfirmed: true, EvidenceRef: "artifact://partial-evidence-and-termination"}, run.UpdatedAt.Add(time.Second))
	if cancelled.Status != StatusCancelled || cancelled.EvidenceRef != "artifact://partial-evidence-and-termination" || cancelled.ResultArtifactRef != "" || cancelled.PRURL != "" {
		t.Fatalf("cancellation evidence/result = %#v", cancelled)
	}
}

func TestCompatibilityResultsMatchLegacyPipelineFieldsAndKeepTaskResultsSeparate(t *testing.T) {
	run := startGeneration(t, newRun(t, false, 6))
	run = generateCandidate(t, run, run.UpdatedAt.Add(time.Second))
	run = apply(t, run, Event{Type: EventFocusedTestCompleted, Succeeded: true, EvidenceRef: "artifact://testgen-run/focused-report"}, run.UpdatedAt.Add(time.Second))

	pipeline, task := run.CompatibilityResults()
	if pipeline.ChainsGitURL != run.Request.RepositoryURL || pipeline.ChainsGitCommit != testCommit ||
		pipeline.TestsAdded != "1" || pipeline.PRURL != "" || pipeline.RunRef != run.RunID ||
		pipeline.TerminalStatus != string(StatusRunning) {
		t.Fatalf("test-generation pipeline projection = %#v", pipeline)
	}
	if task.TestsAdded != "1" || task.Attempts != "1" || task.LastError != "" {
		t.Fatalf("test-generation generation task projection = %#v", task)
	}
	run = apply(t, run, Event{
		Type:        EventFinalVerifyCompleted,
		Succeeded:   true,
		EvidenceRef: "artifact://testgen-run/test-report",
	}, run.UpdatedAt.Add(time.Second))
	run = apply(t, run, Event{
		Type:      EventPRCompleted,
		Succeeded: true,
		PRURL:     "https://gitlab.example/group/project/-/merge_requests/42",
	}, run.UpdatedAt.Add(time.Second))
	pipeline, _ = run.CompatibilityResults()
	if pipeline.PRURL == "" || pipeline.EvidenceRef != "artifact://testgen-run/test-report" || pipeline.TerminalStatus != string(StatusCompleted) {
		t.Fatalf("completed test-generation pipeline projection = %#v", pipeline)
	}
}

func TestStaleRevisionAndOutOfOrderTimestampAreRejected(t *testing.T) {
	run := newRun(t, false, 6)
	if _, err := run.Apply(Event{ID: "stale", ExpectedRevision: run.Revision + 1, Type: EventSourceResolved, ResolvedCommit: testCommit, OccurredAt: run.CreatedAt.Add(time.Second)}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale event error = %v, want revision conflict", err)
	}
	run = apply(t, run, Event{Type: EventSourceResolved, ResolvedCommit: testCommit}, run.CreatedAt.Add(2*time.Second))
	if _, err := run.Apply(Event{ID: "old-time", ExpectedRevision: run.Revision, Type: EventBaselineCompleted, Succeeded: true, OccurredAt: run.CreatedAt.Add(time.Second)}); err == nil {
		t.Fatal("out-of-order event timestamp was accepted")
	}
}

func TestRequestModesRequireExplicitValidArtifactAndCompletionPolicy(t *testing.T) {
	request := newRun(t, false, 3).Request
	for name, mutate := range map[string]func(*Request){
		"unknown input mode":      func(request *Request) { request.InputMode = "generation" },
		"missing artifact digest": func(request *Request) { request.InputMode = InputModeArtifactInput },
		"invalid artifact digest": func(request *Request) {
			request.InputMode = InputModeArtifactInput
			request.InputArtifactDigest = "artifact://uploaded"
		},
		"digest without artifact mode": func(request *Request) { request.InputArtifactDigest = digest([]byte("uploaded")) },
		"unknown completion mode":      func(request *Request) { request.CompletionMode = "pull_request" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := request
			mutate(&invalid)
			if err := invalid.Validate(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("invalid request error = %v", err)
			}
		})
	}
}

func TestRequestDigestAndIdempotencyBindModesAndInputArtifact(t *testing.T) {
	legacy := newRun(t, false, 3)
	artifact := coordinatorRunAtStage(t, StageSource)
	artifact.Request.CompletionMode = ""
	for _, base := range []Run{legacy, artifact} {
		for _, field := range []string{"input", "completion"} {
			t.Run(base.Request.InputMode+"/"+field, func(t *testing.T) {
				request := base.Request
				if field == "completion" {
					request.CompletionMode = CompletionModePatchOnly
				} else if request.InputMode == "" {
					request.InputMode = InputModeArtifactInput
					request.InputArtifactDigest = digest([]byte("first upload"))
				} else {
					request.InputArtifactDigest = digest([]byte("changed upload"))
				}
				original, err := NewRun(base.RunID, base.IdempotencyKey, base.Request, base.MaxAttempts, base.CreatedAt)
				if err != nil {
					t.Fatal(err)
				}
				changed, err := NewRun(base.RunID+"-changed", base.IdempotencyKey, request, base.MaxAttempts, base.CreatedAt)
				if err != nil {
					t.Fatal(err)
				}
				if changed.RequestDigest == original.RequestDigest {
					t.Fatalf("request digest did not bind %s", field)
				}
				repository := newMemoryRepository()
				if _, _, err := repository.CreateRun(context.Background(), original); err != nil {
					t.Fatal(err)
				}
				if _, _, err := repository.CreateRun(context.Background(), changed); !errors.Is(err, ErrIdempotencyConflict) {
					t.Fatalf("changed %s reused idempotency key: %v", field, err)
				}
			})
		}
	}
}

func TestLegacyOmittedModesPreservePersistedJSONDigestsAndReplay(t *testing.T) {
	const oldRequestJSON = `{"repository_url":"https://gitlab.example/group/spring-demo.git","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_branch":"main","issue_iid":"42","subdirectory":"source","profile_id":"spring-java17-maven","profile_version":"0.1.0","profile_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","verify_commit":false}`
	oldDigest := digest([]byte(`{"request":` + oldRequestJSON + `,"max_attempts":6}`))
	oldRunJSON := `{"run_id":"testgen-run-1","idempotency_key":"gitlab:issue:42:rev-1","request_digest":"` + oldDigest + `","request":` + oldRequestJSON + `,"status":"queued","stage":"source","revision":1,"max_attempts":6,"generation_attempts":[],"tests_added":0,"changed_test_paths":[],"pr_url":"","created_at":"2026-11-18T12:00:00Z","updated_at":"2026-11-18T12:00:00Z"}`
	var run Run
	if err := json.Unmarshal([]byte(oldRunJSON), &run); err != nil {
		t.Fatal(err)
	}
	if err := run.Validate(); err != nil {
		t.Fatalf("persisted legacy run rejected: %v", err)
	}
	encoded, err := json.Marshal(run)
	if err != nil || string(encoded) != oldRunJSON || run.RequestDigest != newRun(t, false, 6).RequestDigest {
		t.Fatalf("legacy persisted JSON/digest changed: json=%s err=%v", encoded, err)
	}
	for _, event := range []Event{
		{Type: EventSourceResolved, ResolvedCommit: testCommit},
		{Type: EventBaselineCompleted, Succeeded: true},
		{Type: EventGenerationStarted},
		{Type: EventGenerationCompleted, ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"}, PatchDigest: digest([]byte("legacy patch"))},
		{Type: EventFocusedTestCompleted, Succeeded: true, EvidenceRef: "artifact://focused"},
		{Type: EventFinalVerifyCompleted, Succeeded: true, EvidenceRef: "artifact://verify"},
		{Type: EventPRCompleted, Succeeded: true, PRURL: "https://git.example/pr/42"},
	} {
		run = apply(t, run, event, run.UpdatedAt.Add(time.Second))
		encoded, err = json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		var restored Run
		if err := json.Unmarshal(encoded, &restored); err != nil || restored.Validate() != nil || !reflect.DeepEqual(restored, run) {
			t.Fatalf("legacy replay did not roundtrip: %#v, err=%v", restored, err)
		}
		for _, field := range []string{"input_mode", "input_artifact_digest", "completion_mode", "candidate", "test_artifact_digest", "result_artifact_ref"} {
			if strings.Contains(string(encoded), `"`+field+`"`) {
				t.Fatalf("legacy JSON gained %s: %s", field, encoded)
			}
		}
	}
	if run.Status != StatusCompleted || run.AttemptCount() != 1 || run.PRURL == "" || run.Candidate != nil || run.ResultArtifactRef != "" {
		t.Fatalf("legacy PR flow changed: %#v", run)
	}
	oldEventJSON := `{"event_id":"legacy-source","expected_revision":1,"event_type":"source_resolved","occurred_at":"2026-11-18T12:00:01Z","resolved_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	var oldEvent Event
	if err := json.Unmarshal([]byte(oldEventJSON), &oldEvent); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(oldEvent)
	if err != nil || string(encoded) != oldEventJSON {
		t.Fatalf("legacy event payload/digest changed: %s, %v", encoded, err)
	}
}

func TestArtifactCandidateAcceptanceBindsPathsPatchAndRetrievableArtifact(t *testing.T) {
	pending := coordinatorRunAtStage(t, StageFocusedTest)
	accepted := apply(t, pending, Event{Type: EventFocusedTestCompleted, Succeeded: true, EvidenceRef: "artifact://focused"}, pending.UpdatedAt.Add(time.Second))
	if pending.Candidate.Status != AttemptGenerated || pending.TestsAdded != 0 || accepted.Candidate.Status != AttemptAccepted || accepted.TestArtifactDigest != pending.Candidate.TestArtifactDigest {
		t.Fatalf("artifact acceptance lost isolation/binding: pending=%#v accepted=%#v", pending, accepted)
	}
	for name, mutate := range map[string]func(*Run){
		"accepted paths":     func(run *Run) { run.ChangedTestPaths[0] = "src/test/java/example/OtherTest.java" },
		"accepted patch":     func(run *Run) { run.PatchDigest = digest([]byte("different patch")) },
		"accepted artifact":  func(run *Run) { run.TestArtifactDigest = digest([]byte("different artifact")) },
		"candidate artifact": func(run *Run) { run.Candidate.TestArtifactDigest = digest([]byte("different artifact")) },
		"fake generation attempt": func(run *Run) {
			run.Attempts = []GenerationAttempt{{Number: 1, Status: AttemptAccepted, StartedAt: run.CreatedAt}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			corrupt := cloneRun(accepted)
			mutate(&corrupt)
			if err := corrupt.Validate(); !errors.Is(err, ErrInvalidRun) {
				t.Fatalf("corrupt artifact binding accepted: %#v, err=%v", corrupt, err)
			}
		})
	}
	artifactStage := coordinatorRunAtStage(t, StageArtifactInput)
	if _, err := artifactStage.Apply(Event{ID: "fake-generation", ExpectedRevision: artifactStage.Revision, Type: EventGenerationStarted, OccurredAt: artifactStage.UpdatedAt.Add(time.Second)}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("artifact input created a generation attempt: %v", err)
	}
	if _, err := accepted.Apply(Event{ID: "publish-before-verify", ExpectedRevision: accepted.Revision, Type: EventPatchPublished, Succeeded: true, ResultArtifactRef: "artifact://patch", OccurredAt: accepted.UpdatedAt.Add(time.Second)}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("publication bypassed final verification: %v", err)
	}
}

func TestReconcileOnlyDoesNotChangeOperationInputJSON(t *testing.T) {
	work := WorkRequest{RunID: "run-1", TestArtifactDigest: digest([]byte("artifact")), PatchDigest: digest([]byte("patch"))}
	before, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	work.ReconcileOnly = true
	after, err := json.Marshal(work)
	if err != nil || string(before) != string(after) {
		t.Fatalf("reconciliation changed immutable work digest: before=%s after=%s err=%v", before, after, err)
	}
}

func newRun(t *testing.T, verifyCommit bool, maxAttempts int) Run {
	t.Helper()
	run, err := NewRun("testgen-run-1", "gitlab:issue:42:rev-1", Request{
		RepositoryURL:  "https://gitlab.example/group/spring-demo.git",
		Revision:       testCommit,
		BaseBranch:     "main",
		IssueIID:       "42",
		Subdirectory:   "source",
		ProfileID:      "spring-java17-maven",
		ProfileVersion: "0.1.0",
		ProfileDigest:  "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		VerifyCommit:   verifyCommit,
	}, maxAttempts, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

func startGeneration(t *testing.T, run Run) Run {
	t.Helper()
	now := run.UpdatedAt.Add(time.Second)
	if run.Status == StatusQueued {
		run = apply(t, run, Event{Type: EventSourceResolved, ResolvedCommit: testCommit}, now)
		now = run.UpdatedAt.Add(time.Second)
	}
	if run.Stage == StageBaseline {
		run = apply(t, run, Event{Type: EventBaselineCompleted, Succeeded: true}, now)
	}
	return apply(t, run, Event{Type: EventGenerationStarted}, run.UpdatedAt.Add(time.Second))
}

func generateCandidate(t *testing.T, run Run, at time.Time) Run {
	t.Helper()
	return apply(t, run, Event{
		Type:             EventGenerationCompleted,
		ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"},
		PatchDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, at)
}

func apply(t *testing.T, run Run, event Event, at time.Time) Run {
	t.Helper()
	event.ID = run.RunID + "-event-" + string(rune('a'+run.Revision%26))
	event.ExpectedRevision = run.Revision
	event.OccurredAt = at
	next, err := run.Apply(event)
	if err != nil {
		t.Fatalf("apply %s at revision %d: %v", event.Type, run.Revision, err)
	}
	return next
}
