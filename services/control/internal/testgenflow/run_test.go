package testgenflow

import (
	"errors"
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
