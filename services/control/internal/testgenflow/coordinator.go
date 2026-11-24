package testgenflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrWorkPending = errors.New("test-generation worker operation is pending")

type WorkRequest struct {
	RunID              string
	IdempotencyKey     string
	Request            Request
	ResolvedCommit     string
	Attempt            int
	ChangedTestPaths   []string
	RepairFeedback     string
	TestArtifactDigest string
	PatchDigest        string
	ReconcileOnly      bool               `json:"-"`
	RequestDigest      string             `json:",omitempty"`
	Reviews            []GenerationReview `json:",omitempty"`
}

type MavenPhase string

const (
	MavenPhasePackage      MavenPhase = "package"
	MavenPhaseFocusedTests MavenPhase = "focused_tests"
	MavenPhaseFinalVerify  MavenPhase = "verify"
)

type MavenWorkRequest struct {
	WorkRequest
	Phase MavenPhase
}

type GenerationResult struct {
	ChangedTestPaths   []string
	PatchDigest        string
	TestArtifactDigest string
	Checkpoint         *GenerationCheckpoint `json:",omitempty"`
	Outcome            string                `json:",omitempty"`
	EvidenceRef        string                `json:",omitempty"`
	Diagnostic         string                `json:",omitempty"`
}

type MavenResult struct {
	Succeeded   bool
	EvidenceRef string
	Diagnostic  string
}

type WorkerFailure struct {
	Diagnostic     string
	OutcomeUnknown bool
	Cause          error
}

func (f *WorkerFailure) Error() string {
	if f == nil {
		return "worker operation failed"
	}
	if strings.TrimSpace(f.Diagnostic) != "" {
		return f.Diagnostic
	}
	return "worker operation failed"
}

func (f *WorkerFailure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Cause
}

type SourceProvider interface {
	ResolveSource(context.Context, WorkRequest) (string, error)
}

type SignatureVerifier interface {
	VerifyCommit(context.Context, WorkRequest) (bool, string, error)
}

type GenerationWorker interface {
	GenerateTests(context.Context, WorkRequest) (GenerationResult, error)
}

type ArtifactInputProvider interface {
	PrepareTestArtifact(context.Context, WorkRequest) (GenerationResult, error)
}

type MavenProfileRunner interface {
	RunMaven(context.Context, MavenWorkRequest) (MavenResult, error)
}

type SCMWorker interface {
	OpenTestsPR(context.Context, WorkRequest) (string, error)
}

type PatchPublisher interface {
	PublishPatch(context.Context, WorkRequest) (string, error)
}

type Coordinator struct {
	Repository    Repository
	Source        SourceProvider
	Signature     SignatureVerifier
	Generation    GenerationWorker
	ArtifactInput ArtifactInputProvider
	Maven         MavenProfileRunner
	SCM           SCMWorker
	Publisher     PatchPublisher
	Artifacts     ArtifactReader
	Now           func() time.Time
}

func (c *Coordinator) Advance(ctx context.Context, runID string) (Run, error) {
	if err := ctx.Err(); err != nil {
		return Run{}, err
	}
	if c == nil || c.Repository == nil || c.Now == nil {
		return Run{}, fmt.Errorf("test-generation workflow coordinator is unconfigured")
	}
	run, err := c.Repository.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if err := run.Validate(); err != nil {
		return Run{}, err
	}
	if run.Status == StatusCompleted || run.Status == StatusFailed || run.Status == StatusCancelled || run.Status == StatusOutcomeUnknown || run.Status == StatusRejected || run.Status == StatusBlocked || run.Status == StatusWaitingReview {
		return run, nil
	}
	if run.Status == StatusCancelRequested {
		return run, fmt.Errorf("test-generation run cancellation is awaiting worker termination")
	}

	switch run.Stage {
	case StageSource:
		if c.Source == nil {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "source provider is unconfigured"})
		}
		commit, err := c.Source.ResolveSource(ctx, c.work(run, "source", 0))
		if err != nil {
			return c.fail(ctx, run, workerFailure(err))
		}
		return c.record(ctx, run, Event{Type: EventSourceResolved, ResolvedCommit: commit}, true)

	case StageVerifyCommit:
		if c.Signature == nil {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "source signature verifier is unconfigured"})
		}
		succeeded, diagnostic, err := c.Signature.VerifyCommit(ctx, c.work(run, "verify-commit", 0))
		if err != nil {
			return c.fail(ctx, run, workerFailure(err))
		}
		return c.record(ctx, run, Event{Type: EventSignatureVerified, Succeeded: succeeded, Diagnostic: diagnostic}, true)

	case StageBaseline:
		return c.runMaven(ctx, run, MavenPhasePackage, EventBaselineCompleted)

	case StageGenerate:
		if run.Request.InputMode == InputModeModelGeneration {
			return c.advanceModelGeneration(ctx, run)
		}
		if c.Generation == nil {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "generation worker is unconfigured"})
		}
		started := len(run.Attempts) > 0 && run.Attempts[len(run.Attempts)-1].Status == AttemptRunning
		attempt := run.AttemptCount()
		if !started {
			if attempt >= run.MaxAttempts {
				return Run{}, fmt.Errorf("%w: generation attempts exhausted", ErrInvalidTransition)
			}
			attempt++
		}
		work := c.work(run, "generate", attempt)
		work.ReconcileOnly = started
		work.RepairFeedback = run.LastError
		if run.Request.InputMode == InputModeModelGeneration && attempt > 1 && len(run.Attempts) >= attempt-1 {
			work.RepairFeedback = run.Attempts[attempt-2].LastError
		}
		result, workErr := c.Generation.GenerateTests(ctx, work)
		if ctx.Err() != nil || errors.Is(workErr, ErrWorkPending) || errors.Is(workErr, context.Canceled) || errors.Is(workErr, context.DeadlineExceeded) {
			return c.fail(ctx, run, workerFailure(workErr))
		}
		// Pending work is journaled by the adapter, not as a completed domain event.
		if !started {
			run, err = c.record(ctx, run, Event{Type: EventGenerationStarted}, false)
			if err != nil {
				return Run{}, err
			}
		}
		if workErr != nil {
			return c.fail(ctx, run, workerFailure(workErr))
		}
		return c.record(ctx, run, Event{
			Type:               EventGenerationCompleted,
			ChangedTestPaths:   result.ChangedTestPaths,
			PatchDigest:        result.PatchDigest,
			TestArtifactDigest: result.TestArtifactDigest,
		}, true)

	case StageArtifactInput:
		if c.ArtifactInput == nil {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "artifact input provider is unconfigured"})
		}
		result, err := c.ArtifactInput.PrepareTestArtifact(ctx, c.work(run, "artifact-input", 0))
		if err != nil {
			return c.fail(ctx, run, workerFailure(err))
		}
		return c.record(ctx, run, Event{
			Type:               EventArtifactInputCompleted,
			ChangedTestPaths:   result.ChangedTestPaths,
			PatchDigest:        result.PatchDigest,
			TestArtifactDigest: result.TestArtifactDigest,
		}, true)

	case StageFocusedTest:
		return c.runMaven(ctx, run, MavenPhaseFocusedTests, EventFocusedTestCompleted)

	case StageFinalVerify:
		return c.runMaven(ctx, run, MavenPhaseFinalVerify, EventFinalVerifyCompleted)

	case StageOpenPR:
		if c.SCM == nil {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "SCM worker is unconfigured"})
		}
		work := c.work(run, "open-pr", 0)
		work.ChangedTestPaths = append([]string(nil), run.ChangedTestPaths...)
		prURL, err := c.SCM.OpenTestsPR(ctx, work)
		if err != nil {
			return c.fail(ctx, run, workerFailure(err))
		}
		return c.record(ctx, run, Event{Type: EventPRCompleted, Succeeded: true, PRURL: prURL}, true)

	case StagePublishPatch:
		if c.Publisher == nil {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "patch publisher is unconfigured"})
		}
		ref, err := c.Publisher.PublishPatch(ctx, c.work(run, "publish-patch", run.AttemptCount()))
		if err != nil {
			return c.fail(ctx, run, workerFailure(err))
		}
		return c.record(ctx, run, Event{Type: EventPatchPublished, Succeeded: true, ResultArtifactRef: ref}, true)

	case StageDone:
		return run, nil
	default:
		return Run{}, fmt.Errorf("test-generation run has unsupported stage %q", run.Stage)
	}
}

func (c *Coordinator) advanceModelGeneration(ctx context.Context, run Run) (Run, error) {
	started := len(run.Attempts) > 0 && run.Attempts[len(run.Attempts)-1].Status == AttemptRunning
	if !started {
		return c.record(ctx, run, Event{Type: EventGenerationStarted}, false)
	}
	if c.Generation == nil {
		return c.fail(ctx, run, &WorkerFailure{Diagnostic: "generation worker is unconfigured"})
	}
	work := c.work(run, "generate", run.AttemptCount())
	work.RepairFeedback = run.LastError
	result, err := c.Generation.GenerateTests(ctx, work)
	if err != nil || ctx.Err() != nil {
		return c.fail(ctx, run, workerFailure(err))
	}
	// The staged worker journals each bounded phase independently. Checkpoints
	// and explicit model uncertainty are observations, not worker failures.
	if result.Checkpoint != nil {
		if result.Outcome != "" || len(result.ChangedTestPaths) != 0 || result.PatchDigest != "" || result.TestArtifactDigest != "" {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "worker result failed validation"})
		}
		return c.record(ctx, run, Event{Type: EventGenerationCheckpointRequested, Checkpoint: result.Checkpoint}, true)
	}
	if result.Outcome != "" {
		if len(result.ChangedTestPaths) != 0 || result.PatchDigest != "" || result.TestArtifactDigest != "" {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "worker result failed validation"})
		}
		return c.record(ctx, run, Event{Type: EventGenerationStopped, Outcome: result.Outcome, EvidenceRef: result.EvidenceRef, Diagnostic: result.Diagnostic}, true)
	}
	return c.record(ctx, run, Event{Type: EventGenerationCompleted, ChangedTestPaths: result.ChangedTestPaths, PatchDigest: result.PatchDigest, TestArtifactDigest: result.TestArtifactDigest}, true)
}

func (c *Coordinator) runMaven(ctx context.Context, run Run, phase MavenPhase, eventType EventType) (Run, error) {
	if c.Maven == nil {
		return c.fail(ctx, run, &WorkerFailure{Diagnostic: "Maven profile runner is unconfigured"})
	}
	attempt := 0
	if phase != MavenPhasePackage {
		attempt = run.AttemptCount()
	}
	work := c.work(run, string(phase), attempt)
	result, err := c.Maven.RunMaven(ctx, MavenWorkRequest{WorkRequest: work, Phase: phase})
	if err != nil {
		return c.fail(ctx, run, workerFailure(err))
	}
	if phase == MavenPhaseFocusedTests && result.Succeeded && run.Request.InputMode == InputModeModelGeneration {
		if diagnostic, mismatch := DifferentialOracleMismatch(ctx, c.Artifacts, run, MavenWorkRequest{WorkRequest: work, Phase: phase}, result); mismatch {
			result.Succeeded = false
			result.Diagnostic = diagnostic
		}
	}
	if phase == MavenPhaseFocusedTests && !result.Succeeded && strings.TrimSpace(result.Diagnostic) == "" {
		result.Diagnostic = "focused Maven test command failed"
	}
	event := Event{Type: eventType, Succeeded: result.Succeeded, Diagnostic: result.Diagnostic, EvidenceRef: result.EvidenceRef}
	return c.record(ctx, run, event, true)
}

func (c *Coordinator) fail(ctx context.Context, run Run, failure *WorkerFailure) (Run, error) {
	if err := ctx.Err(); err != nil {
		return run, err
	}
	if errors.Is(failure, ErrWorkPending) || errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
		return run, failure
	}
	diagnostic := "worker operation failed"
	if failure != nil && strings.TrimSpace(failure.Diagnostic) != "" {
		diagnostic = failure.Diagnostic
	}
	eventType := EventStageFailed
	if failure != nil && failure.OutcomeUnknown {
		eventType = EventOutcomeUnknown
	}
	event := Event{Type: eventType, Diagnostic: diagnostic}
	var retained interface{ RetainedEvidence() string }
	if failure != nil && errors.As(failure, &retained) {
		event.EvidenceRef = retained.RetainedEvidence()
	}
	return c.record(ctx, run, event, false)
}

func (c *Coordinator) record(ctx context.Context, run Run, event Event, workerResult bool) (Run, error) {
	if err := ctx.Err(); err != nil {
		return run, err
	}
	eventID, err := NewEventID()
	if err != nil {
		return Run{}, err
	}
	event.ID = eventID
	event.ExpectedRevision = run.Revision
	event.OccurredAt = c.Now().UTC().Truncate(time.Microsecond)
	if event.OccurredAt.Before(run.UpdatedAt) {
		event.OccurredAt = run.UpdatedAt
	}
	if _, err := run.Apply(event); err != nil {
		if workerResult && (errors.Is(err, ErrInvalidEvent) || errors.Is(err, ErrInvalidTransition) || errors.Is(err, ErrInvalidRun)) {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "worker result failed validation"})
		}
		return Run{}, err
	}
	return c.Repository.ApplyEvent(ctx, run.RunID, event)
}

func (c *Coordinator) work(run Run, operation string, attempt int) WorkRequest {
	key := WorkKey(run.RunID, operation, attempt)
	work := WorkRequest{
		RunID:              run.RunID,
		IdempotencyKey:     key,
		Request:            run.Request,
		ResolvedCommit:     run.ResolvedCommit,
		Attempt:            attempt,
		ChangedTestPaths:   append([]string(nil), run.ChangedTestPaths...),
		PatchDigest:        run.PatchDigest,
		TestArtifactDigest: run.TestArtifactDigest,
	}
	if run.Request.InputMode == InputModeModelGeneration {
		isolated := cloneRun(run)
		work.Request = isolated.Request
		work.RequestDigest = run.RequestDigest
		for _, review := range isolated.Reviews {
			if review.Attempt == attempt {
				work.Reviews = append(work.Reviews, review)
			}
		}
	}
	if run.Stage == StageFocusedTest {
		if run.Candidate != nil {
			work.ChangedTestPaths = append([]string(nil), run.Candidate.ChangedTestPaths...)
			work.PatchDigest = run.Candidate.PatchDigest
			work.TestArtifactDigest = run.Candidate.TestArtifactDigest
		} else if len(run.Attempts) > 0 {
			candidate := run.Attempts[len(run.Attempts)-1]
			work.ChangedTestPaths = append([]string(nil), candidate.ChangedTestPaths...)
			work.PatchDigest = candidate.PatchDigest
			work.TestArtifactDigest = candidate.TestArtifactDigest
		}
	}
	return work
}

func WorkKey(runID, operation string, attempt int) string {
	return digest([]byte(runID)) + ":" + operation + ":" + fmt.Sprint(attempt)
}

func workerFailure(err error) *WorkerFailure {
	var failure *WorkerFailure
	if errors.As(err, &failure) {
		return &WorkerFailure{Diagnostic: failure.Diagnostic, OutcomeUnknown: failure.OutcomeUnknown, Cause: err}
	}
	return &WorkerFailure{Diagnostic: "worker operation failed", Cause: err}
}
