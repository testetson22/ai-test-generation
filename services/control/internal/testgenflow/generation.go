package testgenflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var generationTestIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+#[A-Za-z_][A-Za-z0-9_]*$`)

type GenerationRequest struct {
	Objective    string   `json:"objective"`
	ContextPaths []string `json:"context_paths"`
	PolicyDigest string   `json:"policy_digest"`
}

type GenerationCheckpoint struct {
	ID          string    `json:"id"`
	Stage       string    `json:"stage"`
	ArtifactRef string    `json:"artifact_ref"`
	Attempt     int       `json:"attempt"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type GenerationReview struct {
	DecisionID                 string    `json:"decision_id"`
	CheckpointID               string    `json:"checkpoint_id"`
	Stage                      string    `json:"stage"`
	ArtifactRef                string    `json:"artifact_ref"`
	Attempt                    int       `json:"attempt"`
	Action                     string    `json:"action"`
	ReviewerID                 string    `json:"reviewer_id"`
	ReviewerRole               string    `json:"reviewer_role"`
	AuthorizationRef           string    `json:"authorization_ref"`
	Rationale                  string    `json:"rationale"`
	ExpectedTestIDs            []string  `json:"expected_test_ids,omitempty"`
	ApprovedTestArtifactDigest string    `json:"approved_test_artifact_digest,omitempty"`
	DecidedAt                  time.Time `json:"decided_at"`
}

func (g GenerationRequest) Validate() error {
	if strings.TrimSpace(g.Objective) == "" || len(g.Objective) > 8192 || !utf8.ValidString(g.Objective) || strings.ContainsRune(g.Objective, '\x00') || !sha256Pattern.MatchString(g.PolicyDigest) {
		return fmt.Errorf("%w: objective must be 1..8192 bytes and policy must be a SHA256 digest", ErrInvalidRequest)
	}
	if len(g.ContextPaths) < 1 || len(g.ContextPaths) > 64 {
		return fmt.Errorf("%w: context path count must be 1..64", ErrInvalidRequest)
	}
	seen := make(map[string]bool, len(g.ContextPaths))
	for _, value := range g.ContextPaths {
		if !validSubdirectory(value) || len(value) > 1024 || strings.IndexFunc(value, unicode.IsControl) >= 0 || !utf8.ValidString(value) || seen[value] {
			return fmt.Errorf("%w: context paths must be unique bounded normalized repository-relative paths", ErrInvalidRequest)
		}
		seen[value] = true
	}
	return nil
}

// GenerationCheckpointID binds the approval scope to the immutable source,
// profile and policy in RequestDigest, never to an identity chosen by a model.
func GenerationCheckpointID(runID, requestDigest string, attempt int, stage, artifactRef string) string {
	data, _ := json.Marshal([]any{runID, requestDigest, attempt, stage, artifactRef})
	return digest(data)
}

func (c GenerationCheckpoint) Validate(run Run) error {
	if run.Request.InputMode != InputModeModelGeneration || c.Attempt < 1 || c.Attempt != len(run.Attempts) || run.Attempts[c.Attempt-1].Status != AttemptRunning || run.Stage != StageGenerate {
		return fmt.Errorf("%w: checkpoint must bind the currently running model generation attempt", ErrInvalidEvent)
	}
	if c.Stage != "plan" && c.Stage != "code" || !sha256Pattern.MatchString(c.ArtifactRef) || c.ID != GenerationCheckpointID(run.RunID, run.RequestDigest, c.Attempt, c.Stage, c.ArtifactRef) || c.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: checkpoint identity, stage, content-addressed artifact, or expiry is invalid", ErrInvalidEvent)
	}
	return nil
}

func (r GenerationReview) Validate() error {
	if !validFlowIdent(r.DecisionID) || !validFlowIdent(r.CheckpointID) || r.Attempt < 1 || r.Stage != "plan" && r.Stage != "code" || !sha256Pattern.MatchString(r.ArtifactRef) || r.Action != "approve" && r.Action != "reject" || r.DecidedAt.IsZero() {
		return fmt.Errorf("%w: review identity, scope, action, or time is invalid", ErrInvalidEvent)
	}
	if strings.TrimSpace(r.ReviewerID) == "" || strings.TrimSpace(r.ReviewerID) != r.ReviewerID || len(r.ReviewerID) > 256 || strings.IndexFunc(r.ReviewerID, unicode.IsControl) >= 0 || !utf8.ValidString(r.ReviewerID) || r.ReviewerRole != "reviewer" && r.ReviewerRole != "developer" || strings.TrimSpace(r.AuthorizationRef) == "" || len(r.AuthorizationRef) > 2048 || strings.IndexFunc(r.AuthorizationRef, unicode.IsControl) >= 0 || !utf8.ValidString(r.AuthorizationRef) {
		return fmt.Errorf("%w: an attributable authorized reviewer or developer is required", ErrInvalidEvent)
	}
	if strings.TrimSpace(r.Rationale) == "" || len(r.Rationale) > 2048 || !utf8.ValidString(r.Rationale) || strings.IndexFunc(r.Rationale, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: bounded review rationale is required", ErrInvalidEvent)
	}
	if r.Action == "approve" && r.Stage == "code" {
		if len(r.ExpectedTestIDs) < 1 || len(r.ExpectedTestIDs) > 1000 || !sha256Pattern.MatchString(r.ApprovedTestArtifactDigest) {
			return fmt.Errorf("%w: code approval requires independently supplied test IDs and approved artifact digest", ErrInvalidEvent)
		}
		seen := make(map[string]bool, len(r.ExpectedTestIDs))
		for _, id := range r.ExpectedTestIDs {
			if len(id) > 1024 || !generationTestIDPattern.MatchString(id) || seen[id] {
				return fmt.Errorf("%w: expected test IDs must be unique bounded Java class#method identifiers", ErrInvalidEvent)
			}
			seen[id] = true
		}
	} else if len(r.ExpectedTestIDs) != 0 || r.ApprovedTestArtifactDigest != "" {
		return fmt.Errorf("%w: only code approval may supply expected test IDs and an approved test artifact", ErrInvalidEvent)
	}
	return nil
}

func (r Run) ValidateGenerationReview(review GenerationReview, occurredAt time.Time) error {
	if err := review.Validate(); err != nil {
		return err
	}
	if r.Status != StatusWaitingReview || r.Checkpoint == nil || r.Request.InputMode != InputModeModelGeneration {
		return fmt.Errorf("%w: generation review requires a pending checkpoint", ErrInvalidTransition)
	}
	c := r.Checkpoint
	if err := c.Validate(r); err != nil {
		return err
	}
	if review.CheckpointID != c.ID || review.Stage != c.Stage || review.ArtifactRef != c.ArtifactRef || review.Attempt != c.Attempt || !review.DecidedAt.Equal(occurredAt) || occurredAt.Before(r.UpdatedAt) || !occurredAt.Before(c.ExpiresAt) {
		return fmt.Errorf("%w: decision must bind the current unexpired checkpoint and event time", ErrInvalidEvent)
	}
	for _, prior := range r.Reviews {
		if prior.DecisionID == review.DecisionID || prior.CheckpointID == review.CheckpointID {
			return fmt.Errorf("%w: review identity or checkpoint was already decided", ErrInvalidEvent)
		}
	}
	return nil
}

func (r Run) generationApproval(attempt int, stage string) *GenerationReview {
	for i := range r.Reviews {
		if r.Reviews[i].Attempt == attempt && r.Reviews[i].Stage == stage && r.Reviews[i].Action == "approve" {
			return &r.Reviews[i]
		}
	}
	return nil
}

func (r Run) validateGenerationEvent(event Event) error {
	if event.Checkpoint != nil && event.Type != EventGenerationCheckpointRequested || event.Review != nil && event.Type != EventGenerationReviewDecided || event.Outcome != "" && event.Type != EventGenerationStopped {
		return fmt.Errorf("%w: generation payload is not valid for this event type", ErrInvalidEvent)
	}
	if event.Type == EventGenerationCheckpointRequested || event.Type == EventGenerationReviewDecided || event.Type == EventGenerationStopped {
		if event.ResolvedCommit != "" || event.Succeeded || len(event.ChangedTestPaths) != 0 || event.PatchDigest != "" || event.TestArtifactDigest != "" || event.ResultArtifactRef != "" || event.PRURL != "" || event.TerminationConfirmed {
			return fmt.Errorf("%w: generation review or stop cannot carry execution or publication results", ErrInvalidEvent)
		}
	}
	if r.Status == StatusWaitingReview && event.Type != EventGenerationReviewDecided && event.Type != EventCancelRequested {
		return fmt.Errorf("%w: pending generation review permits only a decision or cancellation", ErrInvalidTransition)
	}
	// Check expiry against the supplied instant before projection timestamps are
	// normalized to database precision; replay never consults the wall clock.
	if event.Type == EventGenerationReviewDecided && event.Review != nil {
		if err := r.ValidateGenerationReview(*event.Review, event.OccurredAt); err != nil {
			return err
		}
	}
	if event.Type == EventGenerationCheckpointRequested && event.Checkpoint != nil && !event.Checkpoint.ExpiresAt.After(event.OccurredAt) {
		return fmt.Errorf("%w: checkpoint must expire after it is requested", ErrInvalidEvent)
	}
	if event.Type == EventGenerationCompleted && r.Request.InputMode == InputModeModelGeneration {
		approval := r.generationApproval(len(r.Attempts), "code")
		if r.Checkpoint != nil || approval == nil || !strings.EqualFold(event.TestArtifactDigest, approval.ApprovedTestArtifactDigest) {
			return fmt.Errorf("%w: model output must match this attempt's code-approved test artifact", ErrInvalidTransition)
		}
	}
	return nil
}

func (next *Run) applyGenerationEvent(r Run, event Event) error {
	if r.Request.InputMode != InputModeModelGeneration || r.Stage != StageGenerate || len(r.Attempts) == 0 || r.Attempts[len(r.Attempts)-1].Status != AttemptRunning {
		return fmt.Errorf("%w: generation event requires an active model generation attempt", ErrInvalidTransition)
	}
	switch event.Type {
	case EventGenerationCheckpointRequested:
		if r.Status != StatusRunning || r.Checkpoint != nil || event.Checkpoint == nil {
			return fmt.Errorf("%w: generation checkpoint is not expected", ErrInvalidTransition)
		}
		checkpoint := *event.Checkpoint
		if err := checkpoint.Validate(r); err != nil {
			return err
		}
		if !checkpoint.ExpiresAt.After(event.OccurredAt) || r.generationApproval(checkpoint.Attempt, checkpoint.Stage) != nil || checkpoint.Stage == "code" && r.generationApproval(checkpoint.Attempt, "plan") == nil {
			return fmt.Errorf("%w: checkpoint must be fresh and follow this attempt's plan approval", ErrInvalidTransition)
		}
		next.Checkpoint = &checkpoint
		next.Status = StatusWaitingReview
	case EventGenerationReviewDecided:
		if event.Review == nil {
			return fmt.Errorf("%w: generation review payload is required", ErrInvalidEvent)
		}
		review := *event.Review
		review.ExpectedTestIDs = slices.Clone(review.ExpectedTestIDs)
		next.Reviews = append(next.Reviews, review)
		next.Checkpoint = nil
		next.Status = StatusRunning
		if review.Action == "reject" {
			next.Status = StatusRejected
			next.Attempts[len(next.Attempts)-1].Status = AttemptRejected
			at := event.OccurredAt
			next.Attempts[len(next.Attempts)-1].FinishedAt = &at
		}
	case EventGenerationStopped:
		if r.Status != StatusRunning || r.Checkpoint != nil || event.Outcome != "blocked" && event.Outcome != "inconclusive" {
			return fmt.Errorf("%w: model generation may stop only with an explicit blocked or inconclusive outcome", ErrInvalidTransition)
		}
		diagnostic, err := boundedDiagnostic(event.Diagnostic)
		if err != nil {
			return err
		}
		ref, err := boundedValue(event.EvidenceRef, 2048, "generation stop evidence reference")
		if err != nil || strings.TrimSpace(ref) == "" {
			return fmt.Errorf("%w: stopped generation requires evidence", ErrInvalidEvent)
		}
		next.Status = StatusBlocked
		next.GenerationOutcome = event.Outcome
		next.LastError = diagnostic
		next.EvidenceRef = ref
		next.Attempts[len(next.Attempts)-1].Status = AttemptBlocked
		at := event.OccurredAt
		next.Attempts[len(next.Attempts)-1].FinishedAt = &at
	}
	return nil
}

func (r Run) validateGenerationState() error {
	if r.Request.InputMode != InputModeModelGeneration {
		if r.Checkpoint != nil || len(r.Reviews) != 0 || r.GenerationOutcome != "" || r.Status == StatusWaitingReview || r.Status == StatusRejected || r.Status == StatusBlocked {
			return fmt.Errorf("%w: generation reviews and outcomes require model generation mode", ErrInvalidRun)
		}
		for _, attempt := range r.Attempts {
			if attempt.Status == AttemptBlocked || attempt.Status == AttemptRejected {
				return fmt.Errorf("%w: model attempt outcomes require model generation mode", ErrInvalidRun)
			}
		}
		return nil
	}
	if len(r.Attempts) > 0 && (r.Stage == StageSource || r.Stage == StageVerifyCommit || r.Stage == StageBaseline || r.Stage == StageArtifactInput) {
		return fmt.Errorf("%w: model attempts cannot precede source and baseline preparation", ErrInvalidRun)
	}
	if (r.Status == StatusWaitingReview) != (r.Checkpoint != nil) {
		return fmt.Errorf("%w: waiting review must have exactly one pending checkpoint", ErrInvalidRun)
	}
	if r.Checkpoint != nil {
		if err := r.Checkpoint.Validate(r); err != nil {
			return fmt.Errorf("%w: invalid pending checkpoint: %v", ErrInvalidRun, err)
		}
		if !r.Checkpoint.ExpiresAt.After(r.UpdatedAt) || r.generationApproval(r.Checkpoint.Attempt, r.Checkpoint.Stage) != nil || r.Checkpoint.Stage == "code" && r.generationApproval(r.Checkpoint.Attempt, "plan") == nil {
			return fmt.Errorf("%w: checkpoint is expired, decided, or precedes plan approval", ErrInvalidRun)
		}
	}
	if r.GenerationOutcome != "" && r.GenerationOutcome != "blocked" && r.GenerationOutcome != "inconclusive" || (r.Status == StatusBlocked) != (r.GenerationOutcome != "") {
		return fmt.Errorf("%w: blocked generation requires an explicit outcome", ErrInvalidRun)
	}
	if (r.Status == StatusBlocked || r.Status == StatusRejected) && (r.Stage != StageGenerate || len(r.Attempts) == 0 || r.TestsAdded != 0) {
		return fmt.Errorf("%w: stopped generation cannot execute or publish", ErrInvalidRun)
	}
	if r.Status == StatusBlocked && (strings.TrimSpace(r.EvidenceRef) == "" || strings.TrimSpace(r.LastError) == "") {
		return fmt.Errorf("%w: blocked generation requires diagnostics and evidence", ErrInvalidRun)
	}
	if len(r.Attempts) > 0 {
		last := r.Attempts[len(r.Attempts)-1]
		if r.Status == StatusBlocked && last.Status != AttemptBlocked || r.Status == StatusRejected && last.Status != AttemptRejected {
			return fmt.Errorf("%w: stopped generation must close its active attempt", ErrInvalidRun)
		}
	}
	seen := make(map[string]bool, len(r.Reviews))
	stages := make(map[string]bool, len(r.Reviews))
	rejected := false
	previousAt := r.CreatedAt
	previousAttempt := 0
	for _, review := range r.Reviews {
		key := fmt.Sprint(review.Attempt) + ":" + review.Stage
		if err := review.Validate(); err != nil || rejected || seen[review.DecisionID] || stages[key] || review.Attempt > len(r.Attempts) || review.Attempt < previousAttempt || review.DecidedAt.Before(previousAt) || review.DecidedAt.After(r.UpdatedAt) || review.CheckpointID != GenerationCheckpointID(r.RunID, r.RequestDigest, review.Attempt, review.Stage, review.ArtifactRef) {
			return fmt.Errorf("%w: generation review history is invalid or unbound", ErrInvalidRun)
		}
		attempt := r.Attempts[review.Attempt-1]
		if review.DecidedAt.Before(attempt.StartedAt) || attempt.FinishedAt != nil && review.DecidedAt.After(*attempt.FinishedAt) || review.Stage == "code" && !stages[fmt.Sprint(review.Attempt)+":plan"] {
			return fmt.Errorf("%w: review precedes its attempt or plan approval", ErrInvalidRun)
		}
		seen[review.DecisionID], stages[key] = true, true
		rejected = review.Action == "reject"
		previousAt, previousAttempt = review.DecidedAt, review.Attempt
	}
	if (r.Status == StatusRejected) != rejected {
		return fmt.Errorf("%w: rejected generation must retain its attributable rejection", ErrInvalidRun)
	}
	for _, attempt := range r.Attempts {
		if attempt.Status == AttemptRejected && r.Status != StatusRejected || attempt.Status == AttemptBlocked && r.Status != StatusBlocked || attempt.Status == AttemptNoTests {
			return fmt.Errorf("%w: model attempt outcome differs from generation outcome", ErrInvalidRun)
		}
		if (attempt.Status == AttemptRejected || attempt.Status == AttemptBlocked) && (attempt.Number != len(r.Attempts) || attempt.FinishedAt == nil || len(attempt.ChangedTestPaths) > 0 || attempt.LastError != "") {
			return fmt.Errorf("%w: stopped model attempt cannot claim generated tests or worker failure", ErrInvalidRun)
		}
		if attempt.Status == AttemptRunning && attempt.Number != len(r.Attempts) {
			return fmt.Errorf("%w: only the current generation attempt can be running", ErrInvalidRun)
		}
		approval := r.generationApproval(attempt.Number, "code")
		if len(attempt.ChangedTestPaths) > 0 && (approval == nil || !strings.EqualFold(attempt.TestArtifactDigest, approval.ApprovedTestArtifactDigest)) {
			return fmt.Errorf("%w: generated test artifact lacks this attempt's code approval", ErrInvalidRun)
		}
	}
	return nil
}
