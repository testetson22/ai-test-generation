package testgenflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type WorkRequest struct {
	RunID            string
	IdempotencyKey   string
	Request          Request
	ResolvedCommit   string
	Attempt          int
	ChangedTestPaths []string
	RepairFeedback   string
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
	ChangedTestPaths []string
	PatchDigest      string
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

func (f *WorkerFailure) Unwrap() error { return f.Cause }

type SourceProvider interface {
	ResolveSource(context.Context, WorkRequest) (string, error)
}

type SignatureVerifier interface {
	VerifyCommit(context.Context, WorkRequest) (bool, string, error)
}

type GenerationWorker interface {
	GenerateTests(context.Context, WorkRequest) (GenerationResult, error)
}

type MavenProfileRunner interface {
	RunMaven(context.Context, MavenWorkRequest) (MavenResult, error)
}

type SCMWorker interface {
	OpenTestsPR(context.Context, WorkRequest) (string, error)
}

type Coordinator struct {
	Repository Repository
	Source     SourceProvider
	Signature  SignatureVerifier
	Generation GenerationWorker
	Maven      MavenProfileRunner
	SCM        SCMWorker
	Now        func() time.Time
}

func (c *Coordinator) Advance(ctx context.Context, runID string) (Run, error) {
	if c == nil || c.Repository == nil || c.Now == nil {
		return Run{}, fmt.Errorf("test-generation workflow coordinator is unconfigured")
	}
	run, err := c.Repository.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if run.Status == StatusCompleted || run.Status == StatusFailed || run.Status == StatusCancelled || run.Status == StatusOutcomeUnknown {
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
		if c.Generation == nil {
			return c.fail(ctx, run, &WorkerFailure{Diagnostic: "generation worker is unconfigured"})
		}
		if len(run.Attempts) == 0 || run.Attempts[len(run.Attempts)-1].Status != AttemptRunning {
			run, err = c.record(ctx, run, Event{Type: EventGenerationStarted}, false)
			if err != nil {
				return Run{}, err
			}
		}
		attempt := run.Attempts[len(run.Attempts)-1]
		work := c.work(run, "generate", attempt.Number)
		work.Attempt = attempt.Number
		work.RepairFeedback = run.LastError
		result, err := c.Generation.GenerateTests(ctx, work)
		if err != nil {
			return c.fail(ctx, run, workerFailure(err))
		}
		return c.record(ctx, run, Event{
			Type:             EventGenerationCompleted,
			ChangedTestPaths: result.ChangedTestPaths,
			PatchDigest:      result.PatchDigest,
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

	case StageDone:
		return run, nil
	default:
		return Run{}, fmt.Errorf("test-generation run has unsupported stage %q", run.Stage)
	}
}

func (c *Coordinator) runMaven(ctx context.Context, run Run, phase MavenPhase, eventType EventType) (Run, error) {
	if c.Maven == nil {
		return c.fail(ctx, run, &WorkerFailure{Diagnostic: "Maven profile runner is unconfigured"})
	}
	work := c.work(run, string(phase), 0)
	if phase == MavenPhaseFocusedTests {
		work.Attempt = run.AttemptCount()
		work.ChangedTestPaths = append([]string(nil), run.ChangedTestPaths...)
	}
	result, err := c.Maven.RunMaven(ctx, MavenWorkRequest{WorkRequest: work, Phase: phase})
	if err != nil {
		return c.fail(ctx, run, workerFailure(err))
	}
	if phase == MavenPhaseFocusedTests && !result.Succeeded && strings.TrimSpace(result.Diagnostic) == "" {
		result.Diagnostic = "focused Maven test command failed"
	}
	event := Event{Type: eventType, Succeeded: result.Succeeded, Diagnostic: result.Diagnostic, EvidenceRef: result.EvidenceRef}
	return c.record(ctx, run, event, true)
}

func (c *Coordinator) fail(ctx context.Context, run Run, failure *WorkerFailure) (Run, error) {
	diagnostic := "worker operation failed"
	if failure != nil && strings.TrimSpace(failure.Diagnostic) != "" {
		diagnostic = failure.Diagnostic
	}
	eventType := EventStageFailed
	if failure != nil && failure.OutcomeUnknown {
		eventType = EventOutcomeUnknown
	}
	return c.record(ctx, run, Event{Type: eventType, Diagnostic: diagnostic}, false)
}

func (c *Coordinator) record(ctx context.Context, run Run, event Event, workerResult bool) (Run, error) {
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
	key := digest([]byte(run.RunID)) + ":" + operation + ":" + fmt.Sprint(attempt)
	return WorkRequest{
		RunID:          run.RunID,
		IdempotencyKey: key,
		Request:        run.Request,
		ResolvedCommit: run.ResolvedCommit,
		Attempt:        attempt,
	}
}

func workerFailure(err error) *WorkerFailure {
	var failure *WorkerFailure
	if errors.As(err, &failure) {
		return failure
	}
	return &WorkerFailure{Diagnostic: "worker operation failed", Cause: err}
}
