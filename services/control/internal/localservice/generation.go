package localservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/agentworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/httpapi"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/localworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

type GenerationWorker interface {
	Ready(context.Context) error
	PolicyDigest() string
	RunStage(context.Context, agentworker.StageRequest) (agentworker.StageResult, error)
	ReadCodeArtifact(context.Context, string, []string) ([]byte, error)
	CancelRun(context.Context, string) (string, error)
	ConfirmOperations(context.Context, string, []string) error
	CleanupRun(context.Context, string) error
	RunEvidence(context.Context, string) ([]string, error)
}

type modelGenerationClaims interface {
	ClaimNextModelGenerationRun(context.Context, string, string, string, time.Duration) (*testgenflow.FlowClaim, error)
}

type modelGenerationRecoveryClaims interface {
	ClaimUnknownModelGenerationRun(context.Context, string, string, string, string, int64, time.Duration) (*testgenflow.FlowClaim, error)
}

func (s *Service) now() time.Time {
	if s.config.API.Now != nil {
		return s.config.API.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}

func (s *Service) reconcileUnknownPlan(ctx context.Context, runID string, expectedRevision int64) (testgenflow.Run, error) {
	if s.config.Generation == nil || !s.config.AllowModelCalls {
		return testgenflow.Run{}, agentworker.ErrExternalOperationsDisabled
	}
	claims, ok := s.config.Repository.(modelGenerationRecoveryClaims)
	if !ok {
		return testgenflow.Run{}, errors.New("model-generation recovery claims are unavailable")
	}
	lease := s.config.LeaseDuration
	if lease < time.Minute {
		lease = time.Minute
	}
	claim, err := claims.ClaimUnknownModelGenerationRun(ctx, runID, s.holder, s.config.API.ProfileDigest, s.config.API.GenerationPolicyDigest, expectedRevision, lease)
	if err != nil {
		return testgenflow.Run{}, err
	}
	if claim == nil {
		return testgenflow.Run{}, testgenflow.ErrRevisionConflict
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = s.config.Repository.ReleaseTestGenerationClaim(releaseCtx, *claim)
	}()

	run, err := s.config.Repository.GetRun(ctx, runID)
	if err != nil {
		return testgenflow.Run{}, err
	}
	if run.Status != testgenflow.StatusOutcomeUnknown || run.Stage != testgenflow.StageGenerate || run.Revision != expectedRevision || run.Request.InputMode != testgenflow.InputModeModelGeneration || run.Request.Generation == nil || run.Request.Generation.PolicyDigest != s.config.API.GenerationPolicyDigest || len(run.Attempts) != 1 || run.Attempts[0].Status != testgenflow.AttemptRunning || run.Checkpoint != nil || len(run.Reviews) != 0 || run.LastError != "generation evidence retained but completion acknowledgement unknown" {
		return testgenflow.Run{}, testgenflow.ErrInvalidTransition
	}

	ops, err := s.config.Repository.ListTestGenerationOperations(ctx, runID)
	if err != nil {
		return testgenflow.Run{}, err
	}
	key := testgenflow.WorkKey(run.RunID, "generate", 1) + ":plan"
	var planOp, analyzeOp *testgenflow.Operation
	for i := range ops {
		switch {
		case ops[i].Key == key && ops[i].Kind == "generation:plan":
			planOp = &ops[i]
		case ops[i].Key == testgenflow.WorkKey(run.RunID, "generate", 1)+":analyze" && ops[i].Kind == "generation:analyze":
			analyzeOp = &ops[i]
		}
	}
	if planOp == nil || analyzeOp == nil || analyzeOp.State != "completed" {
		return testgenflow.Run{}, testgenflow.ErrInvalidTransition
	}
	var analysis agentworker.StageResult
	if json.Unmarshal(analyzeOp.Result, &analysis) != nil || analysis.Status != "completed" || analysis.ArtifactRef == "" {
		return testgenflow.Run{}, testgenflow.ErrInvalidTransition
	}
	work := testgenflow.WorkRequest{
		RunID: run.RunID, IdempotencyKey: testgenflow.WorkKey(run.RunID, "generate", 1),
		Request: run.Request, ResolvedCommit: run.ResolvedCommit, Attempt: 1, RequestDigest: run.RequestDigest,
	}
	stageWork := work
	stageWork.IdempotencyKey += ":plan"
	request := agentworker.StageRequest{Work: stageWork, Phase: "plan", AnalysisRef: analysis.ArtifactRef}
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return testgenflow.Run{}, err
	}
	inputDigest := artifact.Digest(requestBytes)
	if planOp.InputDigest != inputDigest || (planOp.State != "unknown" && planOp.State != "prepared" && planOp.State != "completed") || planOp.State == "unknown" && planOp.Diagnostic != "generation evidence retained but completion acknowledgement unknown" {
		return testgenflow.Run{}, testgenflow.ErrOperationConflict
	}
	if _, err := s.config.Artifacts.Get(ctx, analysis.ArtifactRef, httpapi.MaxDevelopmentArtifactBytes); err != nil {
		return testgenflow.Run{}, fmt.Errorf("retained Analyze evidence unavailable: %w", err)
	}
	if _, _, err := s.config.Repository.BeginTestGenerationOperation(ctx, *claim, key, "generation:plan", inputDigest); err != nil {
		return testgenflow.Run{}, err
	}

	var result agentworker.StageResult
	if planOp.State == "completed" {
		if json.Unmarshal(planOp.Result, &result) != nil {
			return testgenflow.Run{}, testgenflow.ErrOperationConflict
		}
	} else {
		request.Work.ReconcileOnly = true
		request.RecoveryOnly = true
		result, err = s.config.Generation.RunStage(ctx, request)
		if err != nil {
			return testgenflow.Run{}, err
		}
		if result.Status != "completed" || result.ArtifactRef == "" {
			return testgenflow.Run{}, errors.New("retained Plan is not a completed generation result")
		}
		resultBytes, err := json.Marshal(result)
		if err != nil {
			return testgenflow.Run{}, err
		}
		if err := s.config.Repository.FinishTestGenerationOperation(ctx, *claim, key, "completed", resultBytes, ""); err != nil {
			return testgenflow.Run{}, err
		}
	}
	if result.Status != "completed" || result.ArtifactRef == "" {
		return testgenflow.Run{}, testgenflow.ErrOperationConflict
	}
	if _, err := s.config.Artifacts.Get(ctx, result.ArtifactRef, httpapi.MaxDevelopmentArtifactBytes); err != nil {
		return testgenflow.Run{}, fmt.Errorf("retained Plan evidence unavailable: %w", err)
	}

	at := s.now()
	if at.Before(run.UpdatedAt) {
		at = run.UpdatedAt
	}
	checkpoint := &testgenflow.GenerationCheckpoint{
		ID:    testgenflow.GenerationCheckpointID(run.RunID, run.RequestDigest, 1, "plan", result.ArtifactRef),
		Stage: "plan", ArtifactRef: result.ArtifactRef, Attempt: 1, ExpiresAt: at.Add(s.config.ReviewTTL),
	}
	eventDigest := artifact.Digest([]byte(key + "\x00" + inputDigest + "\x00" + result.ArtifactRef))
	event := testgenflow.Event{
		ID: "generation-recovered:" + strings.TrimPrefix(eventDigest, "sha256:"), ExpectedRevision: run.Revision,
		Type: testgenflow.EventGenerationRecovered, OccurredAt: at, Checkpoint: checkpoint,
		Recovery: &testgenflow.GenerationRecovery{OperationKey: key, InputDigest: inputDigest},
	}
	return s.config.Repository.ApplyClaimedTestGenerationEvent(ctx, *claim, event)
}

func (s *Service) approveGeneratedCode(ctx context.Context, ref string, ids []string) (string, error) {
	if s.config.Generation == nil || !s.config.AllowModelCalls {
		return "", agentworker.ErrExternalOperationsDisabled
	}
	raw, err := s.config.Generation.ReadCodeArtifact(ctx, ref, ids)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || int64(len(raw)) > s.config.MaxTestArtifactBytes {
		return "", artifact.ErrTooLarge
	}
	if err := s.config.ValidateTestArtifact(raw); err != nil {
		return "", err
	}
	stored, err := s.config.Artifacts.Put(ctx, raw)
	if err != nil {
		return "", err
	}
	got, err := s.config.Artifacts.Get(ctx, stored.Digest, s.config.MaxTestArtifactBytes)
	if err != nil {
		return "", err
	}
	if stored.Digest != artifact.Digest(raw) || !bytes.Equal(raw, got) {
		return "", artifact.ErrCorrupt
	}
	return stored.Digest, nil
}

func isGenerationOperation(kind string) bool {
	return kind == "generation:analyze" || kind == "generation:plan" || kind == "generation:code"
}

func (s *Service) cleanupWorkers(ctx context.Context, runID string) error {
	if s.config.Generation != nil {
		if err := s.config.Generation.CleanupRun(ctx, runID); err != nil {
			return err
		}
	}
	return s.config.Worker.CleanupRun(ctx, runID)
}

func (s *Service) workerEvidence(ctx context.Context, runID string) ([]string, error) {
	refs, err := s.config.Worker.RunEvidence(ctx, runID)
	if err != nil {
		return nil, err
	}
	if s.config.Generation != nil {
		more, err := s.config.Generation.RunEvidence(ctx, runID)
		if err != nil {
			return nil, err
		}
		refs = append(refs, more...)
	}
	slices.Sort(refs)
	return slices.Compact(refs), nil
}

func (o *operations) GenerateTests(ctx context.Context, work testgenflow.WorkRequest) (testgenflow.GenerationResult, error) {
	s := o.service
	if !s.config.AllowModelCalls || s.config.Generation == nil {
		return testgenflow.GenerationResult{}, agentworker.ErrExternalOperationsDisabled
	}
	run, err := s.config.Repository.GetRun(ctx, work.RunID)
	if err != nil {
		return testgenflow.GenerationResult{}, fmt.Errorf("%w: generation run unavailable", testgenflow.ErrWorkPending)
	}
	if run.Request.InputMode != testgenflow.InputModeModelGeneration || run.Request.Generation == nil || run.Request.Generation.PolicyDigest != s.config.API.GenerationPolicyDigest || work.Attempt < 1 || work.Attempt != len(run.Attempts) {
		return testgenflow.GenerationResult{}, errors.New("generation attempt or policy differs from admitted run")
	}
	var analysisRef, planRef, approvedArtifact string
	previous, err := o.previousCode(ctx, work)
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	work.RepairFeedback, err = o.repairFeedback(ctx, run, work)
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	for _, phase := range []string{"analyze", "plan", "code"} {
		stageWork := work
		stageWork.IdempotencyKey = work.IdempotencyKey + ":" + phase
		// Reviews authorize later dispatch in the controller. They are not
		// mutable inputs to already-completed model phases or model context.
		stageWork.Reviews = nil
		request := agentworker.StageRequest{Work: stageWork, Phase: phase, AnalysisRef: analysisRef, PlanRef: planRef, PreviousCandidateRef: previous}
		var result agentworker.StageResult
		err := o.execute(ctx, stageWork.IdempotencyKey, "generation:"+phase, request, func(reconcile bool) (any, error) {
			// An authorized generation attempt is not itself a submitted phase.
			// Only this phase's write-ahead intent controls reconciliation-only mode.
			request.Work.ReconcileOnly = reconcile
			return s.config.Generation.RunStage(ctx, request)
		}, &result)
		if err != nil {
			return testgenflow.GenerationResult{}, err
		}
		if _, err := s.config.Artifacts.Get(ctx, result.ArtifactRef, httpapi.MaxDevelopmentArtifactBytes); err != nil {
			return testgenflow.GenerationResult{}, fmt.Errorf("%w: generation evidence unavailable", testgenflow.ErrWorkPending)
		}
		switch result.Status {
		case "blocked", "inconclusive":
			diagnostic := strings.TrimSpace(strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return ' '
				}
				return r
			}, result.Diagnostic))
			if len(diagnostic) > 1024 {
				diagnostic = diagnostic[:1024]
				for !utf8.ValidString(diagnostic) {
					diagnostic = diagnostic[:len(diagnostic)-1]
				}
			}
			if diagnostic == "" {
				diagnostic = "generation phase reported " + result.Status
			}
			return testgenflow.GenerationResult{Outcome: result.Status, EvidenceRef: result.ArtifactRef, Diagnostic: diagnostic}, nil
		case "completed":
		default:
			return testgenflow.GenerationResult{}, &retainedFailure{ref: result.ArtifactRef, failure: &testgenflow.WorkerFailure{Diagnostic: "generation phase failed; inspect retained evidence"}}
		}
		if phase == "analyze" {
			analysisRef = result.ArtifactRef
			continue
		}
		approved := false
		for _, review := range run.Reviews {
			if review.Attempt != work.Attempt || review.Stage != phase || review.Action != "approve" {
				continue
			}
			if review.ArtifactRef != result.ArtifactRef {
				return testgenflow.GenerationResult{}, &testgenflow.WorkerFailure{Diagnostic: "reviewed generation artifact changed", OutcomeUnknown: true}
			}
			approved = true
			if phase == "code" {
				approvedArtifact = review.ApprovedTestArtifactDigest
			}
		}
		if !approved {
			at := s.now()
			if at.Before(run.UpdatedAt) {
				at = run.UpdatedAt
			}
			checkpoint := &testgenflow.GenerationCheckpoint{
				ID:    testgenflow.GenerationCheckpointID(run.RunID, run.RequestDigest, work.Attempt, phase, result.ArtifactRef),
				Stage: phase, ArtifactRef: result.ArtifactRef, Attempt: work.Attempt, ExpiresAt: at.Add(s.config.ReviewTTL),
			}
			return testgenflow.GenerationResult{Checkpoint: checkpoint}, nil
		}
		if phase == "plan" {
			planRef = result.ArtifactRef
		}
	}
	if approvedArtifact == "" {
		return testgenflow.GenerationResult{}, errors.New("code requires independently reviewed test artifact")
	}
	prepared := work
	prepared.IdempotencyKey = work.IdempotencyKey + ":materialize"
	prepared.TestArtifactDigest = approvedArtifact
	var result testgenflow.GenerationResult
	err = o.execute(ctx, prepared.IdempotencyKey, "generation:materialize", prepared, func(bool) (any, error) { return s.config.Worker.PrepareTestArtifact(ctx, prepared) }, &result)
	return result, err
}

// Repair context is retained data, not permission to change commands or reuse a
// previous approval. Every new candidate still needs both reviews.
func (o *operations) repairFeedback(ctx context.Context, run testgenflow.Run, work testgenflow.WorkRequest) (string, error) {
	if work.Attempt <= 1 {
		return work.RepairFeedback, nil
	}
	previous := run.Attempts[work.Attempt-2]
	if previous.Status != testgenflow.AttemptFailed || previous.TestArtifactDigest == "" || previous.FocusedTestEvidenceRef == "" {
		return "", errors.New("repair requires a prior failed candidate and retained focused evidence")
	}
	raw, err := o.service.config.Artifacts.Get(ctx, previous.FocusedTestEvidenceRef, httpapi.MaxDevelopmentArtifactBytes)
	if err != nil {
		return "", fmt.Errorf("%w: prior focused evidence unavailable", testgenflow.ErrWorkPending)
	}
	clip := func(data []byte, n int, tail bool) string {
		if len(data) > n {
			if tail {
				data = append([]byte("[truncated prefix]\n"), data[len(data)-n:]...)
			} else {
				data = append(bytes.Clone(data[:n]), []byte("\n[truncated suffix]")...)
			}
		}
		return strings.ReplaceAll(string(bytes.ToValidUTF8(data, nil)), "\x00", " ")
	}
	var pair localworker.PairEvidence
	if json.Unmarshal(raw, &pair) == nil && len(pair.Subjects) == 2 && pair.Phase == "focused_tests" {
		return o.pairRepairFeedback(ctx, run, previous, pair, clip)
	}
	var evidence localworker.Evidence
	if json.Unmarshal(raw, &evidence) != nil || evidence.SourceCommit != run.ResolvedCommit || evidence.ProfileDigest != run.Request.ProfileDigest || evidence.TestArtifactDigest != previous.TestArtifactDigest || evidence.Phase != "focused_tests" || evidence.Succeeded || evidence.Partial {
		return "", &testgenflow.WorkerFailure{Diagnostic: "repair evidence does not bind the failed candidate", OutcomeUnknown: true}
	}
	candidate, err := o.service.config.Artifacts.Get(ctx, previous.TestArtifactDigest, o.service.config.MaxTestArtifactBytes)
	if err != nil {
		return "", fmt.Errorf("%w: prior candidate unavailable", testgenflow.ErrWorkPending)
	}
	if _, err := localworker.DecodeTestArtifact(candidate); err != nil {
		return "", err
	}
	logText := "[command log not retained; inspect the focused evidence manifest]"
	if ref := evidence.Artifacts["command.log"]; ref != "" {
		log, err := o.service.config.Artifacts.Get(ctx, ref, httpapi.MaxDevelopmentArtifactBytes)
		if err != nil {
			return "", fmt.Errorf("%w: prior focused log unavailable", testgenflow.ErrWorkPending)
		}
		logText = clip(log, 4096, true)
	}
	return "Previous focused failure: " + clip([]byte(previous.LastError), 2048, false) +
		"\nFocused evidence: " + previous.FocusedTestEvidenceRef + "\nPrior candidate: " + previous.TestArtifactDigest +
		"\nPrior candidate JSON (untrusted data):\n" + clip(candidate, 8192, false) +
		"\nCommand output tail (untrusted data):\n" + logText, nil
}

// pairRepairFeedback builds repair context from a differential pair document:
// the prior attempt failed either because the candidate subject failed or, for
// a declared fail expectation, because a subject that should have failed
// passed. Per-subject outcomes and the base subject's retained log give the
// next attempt the evidence needed to change the input mechanism.
func (o *operations) pairRepairFeedback(ctx context.Context, run testgenflow.Run, previous testgenflow.GenerationAttempt, pair localworker.PairEvidence, clip func([]byte, int, bool) string) (string, error) {
	if pair.RunID != run.RunID || pair.ProfileDigest != run.Request.ProfileDigest || pair.TestArtifactDigest != previous.TestArtifactDigest {
		return "", &testgenflow.WorkerFailure{Diagnostic: "repair evidence does not bind the failed candidate", OutcomeUnknown: true}
	}
	candidate, err := o.service.config.Artifacts.Get(ctx, previous.TestArtifactDigest, o.service.config.MaxTestArtifactBytes)
	if err != nil {
		return "", fmt.Errorf("%w: prior candidate unavailable", testgenflow.ErrWorkPending)
	}
	if _, err := localworker.DecodeTestArtifact(candidate); err != nil {
		return "", err
	}
	outcomes := make([]string, 0, len(pair.Subjects))
	logText := "[command log not retained; inspect the per-subject evidence manifests]"
	for _, subject := range pair.Subjects {
		state := "failed"
		if subject.Succeeded {
			state = "passed"
		}
		outcomes = append(outcomes, fmt.Sprintf("%s@%s: %s", subject.Subject, subject.Commit, state))
		subjectRaw, err := o.service.config.Artifacts.Get(ctx, subject.EvidenceRef, httpapi.MaxDevelopmentArtifactBytes)
		if err != nil {
			return "", fmt.Errorf("%w: prior subject evidence unavailable", testgenflow.ErrWorkPending)
		}
		var evidence localworker.Evidence
		if json.Unmarshal(subjectRaw, &evidence) != nil || evidence.Subject != subject.Subject || evidence.SourceCommit != subject.Commit || evidence.ProfileDigest != run.Request.ProfileDigest || evidence.TestArtifactDigest != previous.TestArtifactDigest || evidence.Phase != "focused_tests" || evidence.Partial {
			return "", &testgenflow.WorkerFailure{Diagnostic: "repair evidence does not bind the failed candidate", OutcomeUnknown: true}
		}
		if subject.Subject == "base" {
			if ref := evidence.Artifacts["command.log"]; ref != "" {
				log, err := o.service.config.Artifacts.Get(ctx, ref, httpapi.MaxDevelopmentArtifactBytes)
				if err != nil {
					return "", fmt.Errorf("%w: prior focused log unavailable", testgenflow.ErrWorkPending)
				}
				logText = clip(log, 4096, true)
			}
		}
	}
	return "Previous focused differential did not satisfy the approved oracle: " + clip([]byte(previous.LastError), 2048, false) +
		"\nPair outcomes: " + strings.Join(outcomes, ", ") +
		"\nFocused evidence: " + previous.FocusedTestEvidenceRef + "\nPrior candidate: " + previous.TestArtifactDigest +
		"\nPrior candidate JSON (untrusted data):\n" + clip(candidate, 8192, false) +
		"\nBase subject command output tail (untrusted data):\n" + logText, nil
}

func (o *operations) previousCode(ctx context.Context, work testgenflow.WorkRequest) (string, error) {
	if work.Attempt <= 1 {
		return "", nil
	}
	ops, err := o.service.config.Repository.ListTestGenerationOperations(ctx, work.RunID)
	if err != nil {
		return "", fmt.Errorf("%w: prior attempt evidence unavailable", testgenflow.ErrWorkPending)
	}
	key := testgenflow.WorkKey(work.RunID, "generate", work.Attempt-1) + ":code"
	for _, op := range ops {
		if op.Key == key && op.Kind == "generation:code" && op.State == "completed" {
			var result agentworker.StageResult
			if err := json.Unmarshal(op.Result, &result); err != nil {
				return "", err
			}
			return result.ArtifactRef, nil
		}
	}
	return "", errors.New("repair attempt lacks prior code evidence")
}
