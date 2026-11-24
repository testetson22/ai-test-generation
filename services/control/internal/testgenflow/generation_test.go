package testgenflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestModelGenerationRequestValidatesBoundedClientInput(t *testing.T) {
	valid := modelRun(t).Request
	for name, mutate := range map[string]func(*Request){
		"missing generation": func(r *Request) { r.Generation = nil },
		"legacy generation":  func(r *Request) { r.InputMode = "" },
		"artifact generation": func(r *Request) {
			r.InputMode, r.InputArtifactDigest = InputModeArtifactInput, digest([]byte("upload"))
		},
		"input artifact":              func(r *Request) { r.InputArtifactDigest = digest([]byte("upload")) },
		"SCM completion":              func(r *Request) { r.CompletionMode = "" },
		"empty objective":             func(r *Request) { r.Generation.Objective = " " },
		"oversized objective":         func(r *Request) { r.Generation.Objective = strings.Repeat("x", 8193) },
		"invalid UTF8":                func(r *Request) { r.Generation.Objective = "\xff" },
		"NUL objective":               func(r *Request) { r.Generation.Objective = "test\x00injection" },
		"missing policy":              func(r *Request) { r.Generation.PolicyDigest = "" },
		"mutable policy":              func(r *Request) { r.Generation.PolicyDigest = "latest" },
		"malformed opaque context":    func(r *Request) { r.Generation.ContextInput = json.RawMessage(`{"unterminated":`) },
		"invalid utf8 opaque context": func(r *Request) { r.Generation.ContextInput = json.RawMessage([]byte{'"', 0xff, '"'}) },
		"oversized opaque context": func(r *Request) {
			r.Generation.ContextInput = json.RawMessage(`"` + strings.Repeat("x", 64*1024) + `"`)
		},
		"missing paths":     func(r *Request) { r.Generation.ContextPaths = nil },
		"too many paths":    func(r *Request) { r.Generation.ContextPaths = make([]string, 65) },
		"duplicate paths":   func(r *Request) { r.Generation.ContextPaths = []string{"pom.xml", "pom.xml"} },
		"absolute path":     func(r *Request) { r.Generation.ContextPaths = []string{"/tmp/source"} },
		"parent path":       func(r *Request) { r.Generation.ContextPaths = []string{".."} },
		"escape path":       func(r *Request) { r.Generation.ContextPaths = []string{"../source"} },
		"noncanonical path": func(r *Request) { r.Generation.ContextPaths = []string{"src/../pom.xml"} },
		"backslash path":    func(r *Request) { r.Generation.ContextPaths = []string{`src\test`} },
		"control path":      func(r *Request) { r.Generation.ContextPaths = []string{"src\ntest"} },
		"oversized path":    func(r *Request) { r.Generation.ContextPaths = []string{strings.Repeat("x", 1025)} },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cloneRun(modelRun(t)).Request
			mutate(&invalid)
			if err := invalid.Validate(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("invalid model request accepted: %v", err)
			}
		})
	}
	discovered := modelRun(t).Request
	discovered.Generation.ContextMode = ContextModeProfileDiscovery
	discovered.Generation.ContextPaths = nil
	discovered.Generation.ContextInput = json.RawMessage(`{"references":[{"path":"src/example/Service.java","reason":"user-supplied hint"}],"provenance":"user"}`)
	if err := discovered.Validate(); err != nil {
		t.Fatalf("profile discovery with opaque supplemental context rejected: %v", err)
	}
	for name, mutate := range map[string]func(*GenerationRequest){
		"discovery with client paths": func(g *GenerationRequest) { g.ContextPaths = []string{"src/main/java/example/Service.java"} },
		"unknown context mode":        func(g *GenerationRequest) { g.ContextMode = "client-selected-files" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := *discovered.Generation
			mutate(&invalid)
			if err := invalid.Validate(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("invalid discovered context accepted: %v", err)
			}
		})
	}
	for _, objective := range []string{"x", strings.Repeat("x", 8192)} {
		valid.Generation.Objective = objective
		if err := valid.Validate(); err != nil {
			t.Fatalf("valid boundary objective rejected: %v", err)
		}
	}
}

func TestGenerationRequestDigestBindsObjectiveContextAndServerPolicy(t *testing.T) {
	base := modelRun(t)
	for _, change := range []func(*GenerationRequest){
		func(g *GenerationRequest) { g.Objective = "different behavior" },
		func(g *GenerationRequest) { g.ContextPaths = []string{"src/main/java"} },
		func(g *GenerationRequest) { g.ContextMode = ContextModeProfileDiscovery; g.ContextPaths = nil },
		func(g *GenerationRequest) { g.ContextInput = json.RawMessage(`{"references":["other"]}`) },
		func(g *GenerationRequest) { g.PolicyDigest = digest([]byte("other policy")) },
	} {
		request := cloneRun(base).Request
		change(request.Generation)
		other, err := NewRun(base.RunID, base.IdempotencyKey, request, base.MaxAttempts, base.CreatedAt)
		if err != nil || other.RequestDigest == base.RequestDigest {
			t.Fatalf("generation input not bound to request: %#v, %v", request, err)
		}
		repository := newMemoryRepository()
		if _, _, err := repository.CreateRun(context.Background(), base); err != nil {
			t.Fatal(err)
		}
		if _, _, err := repository.CreateRun(context.Background(), other); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("generation input reused request identity: %v", err)
		}
	}
}

func TestCheckpointIdentityBindsRunRequestAttemptStageAndArtifact(t *testing.T) {
	run := startGeneration(t, modelRun(t))
	valid := modelCheckpoint(run, "plan")
	for name, mutate := range map[string]func(*GenerationCheckpoint){
		"model chosen ID": func(c *GenerationCheckpoint) { c.ID = "model-approval" },
		"different run": func(c *GenerationCheckpoint) {
			c.ID = GenerationCheckpointID("other-run", run.RequestDigest, c.Attempt, c.Stage, c.ArtifactRef)
		},
		"different request": func(c *GenerationCheckpoint) {
			c.ID = GenerationCheckpointID(run.RunID, digest([]byte("other policy/source/profile")), c.Attempt, c.Stage, c.ArtifactRef)
		},
		"wrong attempt":    func(c *GenerationCheckpoint) { c.Attempt++ },
		"zero attempt":     func(c *GenerationCheckpoint) { c.Attempt = 0 },
		"changed artifact": func(c *GenerationCheckpoint) { c.ArtifactRef = digest([]byte("different artifact")) },
		"mutable artifact": func(c *GenerationCheckpoint) { c.ArtifactRef = "artifact://latest-plan" },
		"invalid stage":    func(c *GenerationCheckpoint) { c.Stage = "execute" },
		"missing expiry":   func(c *GenerationCheckpoint) { c.ExpiresAt = time.Time{} },
		"expired":          func(c *GenerationCheckpoint) { c.ExpiresAt = run.UpdatedAt },
		"code before plan": func(c *GenerationCheckpoint) {
			*c = modelCheckpoint(run, "code")
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := valid
			mutate(&invalid)
			assertGenerationEventRejected(t, run, Event{Type: EventGenerationCheckpointRequested, Checkpoint: &invalid})
		})
	}
	waiting := apply(t, run, Event{Type: EventGenerationCheckpointRequested, Checkpoint: &valid}, run.UpdatedAt.Add(time.Second))
	valid.ArtifactRef = digest([]byte("mutated caller checkpoint"))
	if waiting.Status != StatusWaitingReview || waiting.Stage != StageGenerate || waiting.Checkpoint.ArtifactRef == valid.ArtifactRef {
		t.Fatalf("pending checkpoint lost identity or isolation: %#v", waiting)
	}
	assertGenerationEventRejected(t, waiting, Event{Type: EventGenerationStarted})
	assertGenerationEventRejected(t, waiting, Event{Type: EventGenerationCompleted})
	assertGenerationEventRejected(t, waiting, Event{Type: EventFocusedTestCompleted, Succeeded: true, EvidenceRef: "artifact://fabricated"})
}

func TestUnknownModelRunRecoversOnlyToEvidenceBoundPlanReview(t *testing.T) {
	run := startGeneration(t, modelRun(t))
	unknown := apply(t, run, Event{Type: EventOutcomeUnknown, Diagnostic: "generation evidence retained but completion acknowledgement unknown"}, run.UpdatedAt.Add(time.Second))
	checkpoint := modelCheckpoint(unknown, "plan")
	recovery := GenerationRecovery{OperationKey: WorkKey(unknown.RunID, "generate", 1) + ":plan", InputDigest: digest([]byte("exact Plan operation input"))}
	for name, mutate := range map[string]func(*GenerationCheckpoint, *GenerationRecovery){
		"wrong phase key": func(_ *GenerationCheckpoint, r *GenerationRecovery) {
			r.OperationKey = WorkKey(unknown.RunID, "generate", 1) + ":code"
		},
		"wrong digest":    func(_ *GenerationCheckpoint, r *GenerationRecovery) { r.InputDigest = "not-a-digest" },
		"code checkpoint": func(c *GenerationCheckpoint, _ *GenerationRecovery) { *c = modelCheckpoint(unknown, "code") },
	} {
		t.Run(name, func(t *testing.T) {
			c, r := checkpoint, recovery
			mutate(&c, &r)
			assertGenerationEventRejected(t, unknown, Event{Type: EventGenerationRecovered, Checkpoint: &c, Recovery: &r})
		})
	}
	updated := apply(t, unknown, Event{Type: EventGenerationRecovered, Checkpoint: &checkpoint, Recovery: &recovery}, unknown.UpdatedAt.Add(time.Second))
	if updated.Status != StatusWaitingReview || updated.Checkpoint == nil || updated.Checkpoint.Stage != "plan" || updated.Checkpoint.ArtifactRef != checkpoint.ArtifactRef || updated.Attempts[0].Status != AttemptRunning || updated.TestsAdded != 0 || updated.LastError != "" {
		t.Fatalf("verified Plan recovery bypassed review or fabricated execution: %#v", updated)
	}
	assertGenerationEventRejected(t, unknown, Event{Type: EventGenerationCheckpointRequested, Checkpoint: &checkpoint})
	assertGenerationEventRejected(t, updated, Event{Type: EventGenerationRecovered, Checkpoint: &checkpoint, Recovery: &recovery})
}

func TestGenerationReviewRequiresAttributionAndExactUnexpiredScope(t *testing.T) {
	waiting := waitingModelReview(t, startGeneration(t, modelRun(t)), "plan")
	valid := modelReview(waiting, "approve")
	for name, mutate := range map[string]func(*GenerationReview){
		"missing decision":         func(r *GenerationReview) { r.DecisionID = "" },
		"missing reviewer":         func(r *GenerationReview) { r.ReviewerID = "" },
		"model role":               func(r *GenerationReview) { r.ReviewerRole = "model" },
		"unknown role":             func(r *GenerationReview) { r.ReviewerRole = "administrator" },
		"missing authorization":    func(r *GenerationReview) { r.AuthorizationRef = "" },
		"authorization control":    func(r *GenerationReview) { r.AuthorizationRef = "auth\nforged" },
		"missing rationale":        func(r *GenerationReview) { r.Rationale = " " },
		"wrong checkpoint":         func(r *GenerationReview) { r.CheckpointID = digest([]byte("other checkpoint")) },
		"wrong artifact":           func(r *GenerationReview) { r.ArtifactRef = digest([]byte("other artifact")) },
		"wrong attempt":            func(r *GenerationReview) { r.Attempt++ },
		"wrong stage":              func(r *GenerationReview) { r.Stage = "code" },
		"wrong action":             func(r *GenerationReview) { r.Action = "execute" },
		"wrong decision time":      func(r *GenerationReview) { r.DecidedAt = r.DecidedAt.Add(time.Second) },
		"fabricated plan IDs":      func(r *GenerationReview) { r.ExpectedTestIDs = []string{"example.Test#works"} },
		"fabricated plan artifact": func(r *GenerationReview) { r.ApprovedTestArtifactDigest = digest([]byte("tests")) },
		"invalid decision mode":    func(r *GenerationReview) { r.DecisionMode = "autonomous" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := valid
			mutate(&invalid)
			assertGenerationEventRejected(t, waiting, Event{Type: EventGenerationReviewDecided, Review: &invalid})
		})
	}
	for _, at := range []time.Time{waiting.Checkpoint.ExpiresAt, waiting.Checkpoint.ExpiresAt.Add(time.Second)} {
		expired := valid
		expired.DecidedAt = at
		if err := waiting.ValidateGenerationReview(expired, at); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("expired decision accepted at %s: %v", at, err)
		}
	}
	for _, role := range []string{"reviewer", "developer"} {
		review := valid
		review.ReviewerRole = role
		review.ReviewerID = "reviewer@example.invalid"
		approved := apply(t, waiting, Event{Type: EventGenerationReviewDecided, Review: &review}, review.DecidedAt)
		if approved.Status != StatusRunning || approved.Stage != StageGenerate || approved.Checkpoint != nil || len(approved.Reviews) != 1 || !reflect.DeepEqual(approved.Reviews[0], review) || waiting.Checkpoint == nil {
			t.Fatalf("approval lost attribution/state isolation: %#v", approved)
		}
	}
}

func TestCodeApprovalRequiresIndependentExpectedTestsAndArtifact(t *testing.T) {
	run := approveModelReview(t, waitingModelReview(t, startGeneration(t, modelRun(t)), "plan"))
	assertGenerationEventRejected(t, run, Event{Type: EventGenerationCompleted, ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"}, PatchDigest: digest([]byte("patch")), TestArtifactDigest: digest([]byte("unapproved tests"))})
	checkpoint := modelCheckpoint(run, "plan")
	assertGenerationEventRejected(t, run, Event{Type: EventGenerationCheckpointRequested, Checkpoint: &checkpoint})
	waiting := waitingModelReview(t, run, "code")
	valid := modelReview(waiting, "approve")
	for name, mutate := range map[string]func(*GenerationReview){
		"no expected IDs":       func(r *GenerationReview) { r.ExpectedTestIDs = nil },
		"empty test ID":         func(r *GenerationReview) { r.ExpectedTestIDs = []string{""} },
		"wildcard selector":     func(r *GenerationReview) { r.ExpectedTestIDs = []string{"example.Test#*"} },
		"class without package": func(r *GenerationReview) { r.ExpectedTestIDs = []string{"Test#works"} },
		"duplicate test ID":     func(r *GenerationReview) { r.ExpectedTestIDs = []string{"example.Test#works", "example.Test#works"} },
		"oversized test ID": func(r *GenerationReview) {
			r.ExpectedTestIDs = []string{"example." + strings.Repeat("T", 1024) + "#works"}
		},
		"no approved artifact":      func(r *GenerationReview) { r.ApprovedTestArtifactDigest = "" },
		"mutable approved artifact": func(r *GenerationReview) { r.ApprovedTestArtifactDigest = "artifact://tests" },
		"rejection with test IDs":   func(r *GenerationReview) { r.Action = "reject" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := valid
			mutate(&invalid)
			assertGenerationEventRejected(t, waiting, Event{Type: EventGenerationReviewDecided, Review: &invalid})
		})
	}
	run = apply(t, waiting, Event{Type: EventGenerationReviewDecided, Review: &valid}, valid.DecidedAt)
	valid.ExpectedTestIDs[0] = "example.MutatedTest#forged"
	if run.Reviews[1].ExpectedTestIDs[0] != "example.GeneratedTest#works" {
		t.Fatal("caller mutated persisted expected test identity")
	}
	result := artifactCandidateResult()
	assertGenerationEventRejected(t, run, Event{Type: EventGenerationCompleted, ChangedTestPaths: result.ChangedTestPaths, PatchDigest: result.PatchDigest, TestArtifactDigest: digest([]byte("not approved"))})
	run = completeModelGeneration(t, run)
	if run.Stage != StageFocusedTest || run.TestsAdded != 0 || run.Attempts[0].Status != AttemptGenerated {
		t.Fatalf("approved generation did not preserve pending candidate gate: %#v", run)
	}
}

func TestRepairAttemptCannotReusePreviousPlanOrCodeApproval(t *testing.T) {
	run := startGeneration(t, modelRun(t))
	for attempt := 1; attempt <= 2; attempt++ {
		assertGenerationEventRejected(t, run, Event{Type: EventGenerationCompleted, ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"}, PatchDigest: digest([]byte("patch")), TestArtifactDigest: digest([]byte("approved tests"))})
		for _, stage := range []string{"plan", "code"} {
			checkpoint := modelCheckpoint(run, stage)
			if attempt == 2 {
				previous := run.Reviews[len(run.Reviews)-2]
				if checkpoint.ID == previous.CheckpointID {
					t.Fatal("repair reused previous checkpoint identity")
				}
			}
			run = waitingModelReview(t, run, stage)
			if attempt == 2 {
				stale := run.Reviews[0]
				stale.DecidedAt = run.UpdatedAt.Add(time.Second)
				assertGenerationEventRejected(t, run, Event{Type: EventGenerationReviewDecided, Review: &stale})
			}
			run = approveModelReview(t, run)
		}
		run = completeModelGeneration(t, run)
		if attempt == 1 {
			run = apply(t, run, Event{Type: EventFocusedTestCompleted, Diagnostic: "independent assertion failed", EvidenceRef: "artifact://focused-failed"}, run.UpdatedAt.Add(time.Second))
			if run.Stage != StageGenerate || run.Status != StatusRunning {
				t.Fatalf("repair not authorized: %#v", run)
			}
			run = apply(t, run, Event{Type: EventGenerationStarted}, run.UpdatedAt.Add(time.Second))
			work := (&Coordinator{}).work(run, "generate", 2)
			if len(work.Reviews) != 0 || work.RepairFeedback != "" || work.Attempt != 2 || work.IdempotencyKey != WorkKey(run.RunID, "generate", 2) {
				t.Fatalf("repair inherited approval scope: %#v", work)
			}
		}
	}
	if run.AttemptCount() != 2 || len(run.Reviews) != 4 || run.Attempts[0].Status != AttemptFailed || run.Attempts[1].Status != AttemptGenerated {
		t.Fatalf("repair lost independent attempts/reviews: %#v", run)
	}
}

func TestGenerationRejectionAndWaitingCancellationNeverExecute(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			run := startGeneration(t, modelRun(t))
			if stage == "code" {
				run = approveModelReview(t, waitingModelReview(t, run, "plan"))
			}
			waiting := waitingModelReview(t, run, stage)
			review := modelReview(waiting, "reject")
			rejected := apply(t, waiting, Event{Type: EventGenerationReviewDecided, Review: &review}, review.DecidedAt)
			if rejected.Status != StatusRejected || rejected.Checkpoint != nil || rejected.TestsAdded != 0 || rejected.Attempts[0].Status != AttemptRejected || rejected.Attempts[0].FinishedAt == nil || rejected.Attempts[0].LastError != "" {
				t.Fatalf("rejection invented worker failure/execution: %#v", rejected)
			}
			assertGenerationEventRejected(t, rejected, Event{Type: EventGenerationStarted})
			repository := newMemoryRepository()
			if _, _, err := repository.CreateRun(context.Background(), rejected); err != nil {
				t.Fatal(err)
			}
			workers := &fakeTestGenerationWorkers{}
			coordinator := &Coordinator{Repository: repository, Generation: workers, Maven: workers, Now: func() time.Time { return rejected.UpdatedAt }}
			if unchanged, err := coordinator.Advance(context.Background(), rejected.RunID); err != nil || !reflect.DeepEqual(unchanged, rejected) || len(workers.calls) != 0 {
				t.Fatalf("rejection invoked execution: %#v, %v", unchanged, err)
			}
			cancelled := apply(t, waiting, Event{Type: EventCancelRequested}, waiting.UpdatedAt.Add(time.Second))
			if cancelled.Status != StatusCancelRequested || cancelled.Checkpoint != nil {
				t.Fatalf("waiting cancellation did not close checkpoint: %#v", cancelled)
			}
			cancelled = apply(t, cancelled, Event{Type: EventCancelled, TerminationConfirmed: true}, cancelled.UpdatedAt.Add(time.Second))
			if cancelled.Status != StatusCancelled || cancelled.TestsAdded != 0 || cancelled.PRURL != "" || cancelled.ResultArtifactRef != "" {
				t.Fatalf("waiting cancellation executed/published: %#v", cancelled)
			}
		})
	}
}

func TestGenerationBlockedAndInconclusiveAreExplicitNotWorkerFailures(t *testing.T) {
	run := startGeneration(t, modelRun(t))
	for _, outcome := range []string{"blocked", "inconclusive"} {
		stopped := apply(t, run, Event{Type: EventGenerationStopped, Outcome: outcome, Diagnostic: "insufficient attributable context", EvidenceRef: "artifact://generation-evidence"}, run.UpdatedAt.Add(time.Second))
		if stopped.Status != StatusBlocked || stopped.GenerationOutcome != outcome || stopped.Stage != StageGenerate || stopped.Attempts[0].Status != AttemptBlocked || stopped.Attempts[0].FinishedAt == nil || stopped.Attempts[0].LastError != "" || stopped.EvidenceRef != "artifact://generation-evidence" {
			t.Fatalf("explicit model stop misclassified: %#v", stopped)
		}
		assertGenerationEventRejected(t, stopped, Event{Type: EventGenerationStarted})
	}
	for _, event := range []Event{
		{Type: EventGenerationStopped, Outcome: "success", Diagnostic: "claimed", EvidenceRef: "artifact://evidence"},
		{Type: EventGenerationStopped, Outcome: "blocked", EvidenceRef: "artifact://evidence"},
		{Type: EventGenerationStopped, Outcome: "blocked", Diagnostic: "missing context"},
		{Type: EventGenerationStopped, Outcome: "blocked", Diagnostic: "missing context", EvidenceRef: " "},
	} {
		assertGenerationEventRejected(t, run, event)
	}
	assertGenerationEventRejected(t, startGeneration(t, newRun(t, false, 3)), Event{Type: EventGenerationStopped, Outcome: "blocked", Diagnostic: "not a model run", EvidenceRef: "artifact://evidence"})
}

func TestGenerationDecisionReplayBindsFullPayloadAndUsesHistoricalEventTime(t *testing.T) {
	ctx := context.Background()
	waiting := waitingModelReview(t, startGeneration(t, modelRun(t)), "plan")
	repository := newMemoryRepository()
	if _, _, err := repository.CreateRun(ctx, waiting); err != nil {
		t.Fatal(err)
	}
	review := modelReview(waiting, "approve")
	event := Event{ID: "durable-decision-event", ExpectedRevision: waiting.Revision, Type: EventGenerationReviewDecided, Review: &review, OccurredAt: review.DecidedAt}
	approved, err := repository.ApplyEvent(ctx, waiting.RunID, event)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []Run{waiting, approved} {
		encoded, err := json.Marshal(state)
		var restored Run
		if err != nil || json.Unmarshal(encoded, &restored) != nil || restored.Validate() != nil || !reflect.DeepEqual(state, restored) {
			t.Fatalf("historical state failed deterministic replay: %s, %v", encoded, err)
		}
	}
	if replay, err := repository.ApplyEvent(ctx, waiting.RunID, event); err != nil || !reflect.DeepEqual(replay, approved) {
		t.Fatalf("historical decision replay mutated state: %#v, %v", replay, err)
	}
	for _, mutate := range []func(*GenerationReview){
		func(r *GenerationReview) { r.ReviewerID = "other-human" },
		func(r *GenerationReview) { r.AuthorizationRef = "auth://different" },
		func(r *GenerationReview) { r.Rationale = "different rationale" },
		func(r *GenerationReview) { r.ArtifactRef = digest([]byte("different artifact")) },
	} {
		changed := review
		mutate(&changed)
		conflict := event
		conflict.Review = &changed
		if _, err := repository.ApplyEvent(ctx, waiting.RunID, conflict); !errors.Is(err, ErrEventConflict) {
			t.Fatalf("changed decision payload replay accepted: %v", err)
		}
	}
	assertGenerationEventRejected(t, approved, Event{Type: EventGenerationReviewDecided, Review: &review})
}

func TestGenerationDecisionExpiryUsesOriginalInstantBeforeMicrosecondProjection(t *testing.T) {
	waiting := waitingModelReview(t, startGeneration(t, modelRun(t)), "plan")
	waiting.Checkpoint.ExpiresAt = waiting.UpdatedAt.Add(time.Second + 500*time.Nanosecond)
	for _, offset := range []time.Duration{400 * time.Nanosecond, 500 * time.Nanosecond, 600 * time.Nanosecond} {
		review := modelReview(waiting, "approve")
		review.DecidedAt = waiting.UpdatedAt.Add(time.Second + offset)
		updated, err := waiting.Apply(Event{ID: "precise-review", ExpectedRevision: waiting.Revision, Type: EventGenerationReviewDecided, Review: &review, OccurredAt: review.DecidedAt})
		if offset >= 500*time.Nanosecond {
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("expired submicrosecond decision accepted: offset=%s err=%v", offset, err)
			}
		} else if err != nil || updated.Status != StatusRunning || !updated.Reviews[0].DecidedAt.Equal(review.DecidedAt.Truncate(time.Microsecond)) {
			t.Fatalf("unexpired decision failed normalization: %#v, %v", updated, err)
		}
	}
}

func TestModelCoordinatorStartsDurablyAndWaitsForBothReviewsBeforeTestExecution(t *testing.T) {
	ctx := context.Background()
	run := modelRun(t)
	run = apply(t, run, Event{Type: EventSourceResolved, ResolvedCommit: testCommit}, run.UpdatedAt.Add(time.Second))
	run = apply(t, run, Event{Type: EventBaselineCompleted, Succeeded: true}, run.UpdatedAt.Add(time.Second))
	repository := newMemoryRepository()
	if _, _, err := repository.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	workers := &fakeTestGenerationWorkers{operationErr: ErrWorkPending, maven: []MavenResult{{Succeeded: true, EvidenceRef: "artifact://focused"}, {Succeeded: true, EvidenceRef: "artifact://verify"}}, resultArtifact: "artifact://published"}
	coordinator := &Coordinator{Repository: repository, Generation: workers, Maven: workers, Publisher: workers, Now: func() time.Time { return run.UpdatedAt }}
	started, err := coordinator.Advance(ctx, run.RunID)
	if err != nil || started.Revision != run.Revision+1 || started.AttemptCount() != 1 || len(workers.calls) != 0 {
		t.Fatalf("initial attempt invoked side effects before durable start: %#v calls=%v err=%v", started, workers.calls, err)
	}
	run = started
	beforeEvents := len(repository.events[run.RunID])
	pending, err := coordinator.Advance(ctx, run.RunID)
	if !errors.Is(err, ErrWorkPending) || !reflect.DeepEqual(pending, run) || len(repository.events[run.RunID]) != beforeEvents {
		t.Fatalf("pending work appended events: %#v, %v", pending, err)
	}
	workers.operationErr = nil
	for _, stage := range []string{"plan", "code"} {
		checkpoint := modelCheckpoint(run, stage)
		workers.generation = []GenerationResult{{Checkpoint: &checkpoint}}
		run, err = coordinator.Advance(ctx, run.RunID)
		if err != nil || run.Status != StatusWaitingReview || run.Stage != StageGenerate || len(workers.mavenRequests) != 0 {
			t.Fatalf("checkpoint advanced to execution: %#v calls=%v err=%v", run, workers.calls, err)
		}
		calls := len(workers.calls)
		paused, err := coordinator.Advance(ctx, run.RunID)
		if err != nil || !reflect.DeepEqual(paused, run) || len(workers.calls) != calls {
			t.Fatalf("paused review invoked worker: %#v, %v", paused, err)
		}
		review := modelReview(run, "approve")
		run, err = repository.ApplyEvent(ctx, run.RunID, Event{ID: stage + "-review", ExpectedRevision: run.Revision, Type: EventGenerationReviewDecided, Review: &review, OccurredAt: review.DecidedAt})
		if err != nil {
			t.Fatal(err)
		}
	}
	result := artifactCandidateResult()
	result.TestArtifactDigest = digest([]byte("approved tests"))
	workers.generation = []GenerationResult{result}
	for _, stage := range []Stage{StageFocusedTest, StageFinalVerify, StagePublishPatch, StageDone} {
		run, err = coordinator.Advance(ctx, run.RunID)
		if err != nil || run.Stage != stage {
			t.Fatalf("advance to %s: %#v, %v", stage, run, err)
		}
	}
	if run.Status != StatusCompleted || run.PRURL != "" || run.TestsAdded != 1 || len(workers.mavenRequests) != 2 || len(workers.publications) != 1 || workers.publications[0].IdempotencyKey != WorkKey(run.RunID, "publish-patch", 1) {
		t.Fatalf("model completion lost patch-only execution boundary: %#v calls=%v", run, workers.calls)
	}
	for i, count := range []int{0, 0, 1, 2} {
		work := workers.generations[i]
		if work.RequestDigest != run.RequestDigest || len(work.Reviews) != count || work.ReconcileOnly || work.Attempt != 1 {
			t.Fatalf("staged worker approval scope %d: %#v", i, work)
		}
	}
}

func TestModelCoordinatorRecordsExplicitStopsAndNeverRestartsTerminals(t *testing.T) {
	for _, outcome := range []string{"blocked", "inconclusive"} {
		run := startGeneration(t, modelRun(t))
		repository := newMemoryRepository()
		if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		workers := &fakeTestGenerationWorkers{generation: []GenerationResult{{Outcome: outcome, Diagnostic: "bounded stop", EvidenceRef: "artifact://stopped"}}}
		coordinator := &Coordinator{Repository: repository, Generation: workers, Now: func() time.Time { return run.UpdatedAt }}
		stopped, err := coordinator.Advance(context.Background(), run.RunID)
		if err != nil || stopped.Status != StatusBlocked || stopped.GenerationOutcome != outcome {
			t.Fatalf("model stop was worker failure: %#v, %v", stopped, err)
		}
		again, err := coordinator.Advance(context.Background(), run.RunID)
		if err != nil || !reflect.DeepEqual(again, stopped) || len(workers.calls) != 1 {
			t.Fatalf("terminal stop invoked model again: %#v calls=%v err=%v", again, workers.calls, err)
		}
	}
}

func TestModelCoordinatorPendingAndCanceledPhasesNeverAppendEvents(t *testing.T) {
	for _, behavior := range []string{"pending", "wrapped pending", "returned cancellation", "returned deadline", "canceled during success", "canceled during failure"} {
		t.Run(behavior, func(t *testing.T) {
			run := startGeneration(t, modelRun(t))
			repository := newMemoryRepository()
			if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			workers := &fakeTestGenerationWorkers{generation: []GenerationResult{{}}}
			wantErr := ErrWorkPending
			switch behavior {
			case "pending":
				workers.operationErr = ErrWorkPending
			case "wrapped pending":
				workers.operationErr = errors.Join(ErrWorkPending, &WorkerFailure{OutcomeUnknown: true, Diagnostic: "still running"})
			case "returned cancellation":
				workers.operationErr, wantErr = context.Canceled, context.Canceled
			case "returned deadline":
				workers.operationErr, wantErr = context.DeadlineExceeded, context.DeadlineExceeded
			case "canceled during success":
				workers.cancel, wantErr = cancel, context.Canceled
			case "canceled during failure":
				workers.cancel, workers.operationErr, wantErr = cancel, errors.New("interrupted"), context.Canceled
			}
			coordinator := &Coordinator{Repository: repository, Generation: workers, Now: func() time.Time { return run.UpdatedAt }}
			unchanged, err := coordinator.Advance(ctx, run.RunID)
			if !errors.Is(err, wantErr) || !reflect.DeepEqual(unchanged, run) || len(repository.events[run.RunID]) != 0 || len(workers.calls) != 1 {
				t.Fatalf("pending/canceled model phase changed state: %#v events=%v err=%v", unchanged, repository.events, err)
			}
		})
	}
}

func TestModelCoordinatorRejectsAmbiguousOrUnapprovedWorkerResults(t *testing.T) {
	for _, result := range []GenerationResult{
		{},
		artifactCandidateResult(),
		{Checkpoint: &GenerationCheckpoint{ID: "model-decides-approval"}},
		{Checkpoint: &GenerationCheckpoint{}, Outcome: "blocked"},
		{Checkpoint: &GenerationCheckpoint{}, TestArtifactDigest: digest([]byte("fabricated"))},
		{Outcome: "blocked", Diagnostic: "missing evidence"},
		{Outcome: "inconclusive", EvidenceRef: "artifact://evidence"},
		{Outcome: "success", Diagnostic: "invented outcome", EvidenceRef: "artifact://evidence"},
		{Outcome: "blocked", Diagnostic: "blocked", EvidenceRef: "artifact://evidence", PatchDigest: digest([]byte("fabricated patch"))},
	} {
		run := startGeneration(t, modelRun(t))
		repository := newMemoryRepository()
		if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		workers := &fakeTestGenerationWorkers{generation: []GenerationResult{result}}
		coordinator := &Coordinator{Repository: repository, Generation: workers, Maven: workers, Now: func() time.Time { return run.UpdatedAt }}
		failed, err := coordinator.Advance(context.Background(), run.RunID)
		if err != nil || failed.Status != StatusFailed || failed.Stage != StageGenerate || failed.TestsAdded != 0 || len(workers.mavenRequests) != 0 || failed.LastError != "worker result failed validation" {
			t.Fatalf("invalid worker result advanced model execution: result=%#v run=%#v err=%v", result, failed, err)
		}
	}
}

func TestGenerationStateValidationRejectsTamperedApprovalBindings(t *testing.T) {
	run := startGeneration(t, modelRun(t))
	run = approveModelReview(t, waitingModelReview(t, run, "plan"))
	run = approveModelReview(t, waitingModelReview(t, run, "code"))
	run = completeModelGeneration(t, run)
	for name, mutate := range map[string]func(*Run){
		"missing review":           func(r *Run) { r.Reviews = nil },
		"wrong code digest":        func(r *Run) { r.Reviews[1].ApprovedTestArtifactDigest = digest([]byte("different tests")) },
		"wrong checkpoint binding": func(r *Run) { r.Reviews[0].ArtifactRef = digest([]byte("different plan")) },
		"wrong reviewer role":      func(r *Run) { r.Reviews[0].ReviewerRole = "model" },
		"code without plan":        func(r *Run) { r.Reviews = r.Reviews[1:] },
		"duplicate decision ID":    func(r *Run) { r.Reviews[1].DecisionID = r.Reviews[0].DecisionID },
		"future decision":          func(r *Run) { r.Reviews[1].DecidedAt = r.UpdatedAt.Add(time.Second) },
		"fake waiting status":      func(r *Run) { r.Status = StatusWaitingReview },
		"unexplained block":        func(r *Run) { r.Status = StatusBlocked },
		"unexplained reject":       func(r *Run) { r.Status = StatusRejected },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cloneRun(run)
			mutate(&invalid)
			if err := invalid.Validate(); !errors.Is(err, ErrInvalidRun) {
				t.Fatalf("corrupt generation approval accepted: %v", err)
			}
		})
	}
}

func modelRun(t *testing.T) Run {
	t.Helper()
	base := newRun(t, false, 3)
	request := base.Request
	request.InputMode = InputModeModelGeneration
	request.CompletionMode = CompletionModePatchOnly
	request.Generation = &GenerationRequest{Objective: "Assert the independently specified behavior", ContextPaths: []string{"pom.xml", "src/main/java"}, PolicyDigest: digest([]byte("server generation policy"))}
	run, err := NewRun(base.RunID, base.IdempotencyKey, request, base.MaxAttempts, base.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func modelCheckpoint(run Run, stage string) GenerationCheckpoint {
	artifactRef := digest([]byte(stage + " artifact"))
	return GenerationCheckpoint{ID: GenerationCheckpointID(run.RunID, run.RequestDigest, run.AttemptCount(), stage, artifactRef), Stage: stage, ArtifactRef: artifactRef, Attempt: run.AttemptCount(), ExpiresAt: run.UpdatedAt.Add(time.Hour)}
}

func waitingModelReview(t *testing.T, run Run, stage string) Run {
	t.Helper()
	checkpoint := modelCheckpoint(run, stage)
	return apply(t, run, Event{Type: EventGenerationCheckpointRequested, Checkpoint: &checkpoint}, run.UpdatedAt.Add(time.Second))
}

func modelReview(run Run, action string) GenerationReview {
	c := run.Checkpoint
	review := GenerationReview{DecisionID: fmt.Sprintf("decision-%d-%s", c.Attempt, c.Stage), CheckpointID: c.ID, Stage: c.Stage, ArtifactRef: c.ArtifactRef, Attempt: c.Attempt, Action: action, ReviewerID: "human-reviewer", ReviewerRole: "reviewer", AuthorizationRef: "auth://server-policy/reviewer", Rationale: "Reviewed the exact immutable artifact and expected behavior", DecidedAt: run.UpdatedAt.Add(time.Second)}
	if c.Stage == "code" && action == "approve" {
		review.ExpectedTestIDs = []string{"example.GeneratedTest#works"}
		review.ApprovedTestArtifactDigest = digest([]byte("approved tests"))
	}
	return review
}

func approveModelReview(t *testing.T, run Run) Run {
	t.Helper()
	review := modelReview(run, "approve")
	return apply(t, run, Event{Type: EventGenerationReviewDecided, Review: &review}, review.DecidedAt)
}

func completeModelGeneration(t *testing.T, run Run) Run {
	t.Helper()
	return apply(t, run, Event{Type: EventGenerationCompleted, ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"}, PatchDigest: digest([]byte("approved patch")), TestArtifactDigest: digest([]byte("approved tests"))}, run.UpdatedAt.Add(time.Second))
}

func assertGenerationEventRejected(t *testing.T, run Run, event Event) {
	t.Helper()
	event.ID, event.ExpectedRevision, event.OccurredAt = "invalid-generation-event", run.Revision, run.UpdatedAt.Add(time.Second)
	before := cloneRun(run)
	if _, err := run.Apply(event); err == nil {
		t.Fatalf("invalid %s accepted: %#v", event.Type, event)
	}
	if !reflect.DeepEqual(run, before) {
		t.Fatal("rejected event mutated caller state")
	}
}
