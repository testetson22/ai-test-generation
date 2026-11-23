package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

var generationReviewIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
var generationReviewTestIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+#[A-Za-z_][A-Za-z0-9_]*$`)

func ValidDevelopmentReviewer(config TestGenerationDevelopmentConfig) bool {
	for _, value := range []string{config.ReviewerID, config.ReviewerRole, config.ReviewAuthorizationRef} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 2048 || strings.IndexFunc(value, unicode.IsControl) >= 0 || !utf8.ValidString(value) {
			return false
		}
	}
	return generationReviewIDPattern.MatchString(config.ReviewerID) && (config.ReviewerRole == "developer" || config.ReviewerRole == "reviewer")
}

func (a *TestGenerationDevelopmentAPI) reviewGeneration(w http.ResponseWriter, r *http.Request) {
	if !a.Ready() {
		unavailableTestGenerationDevelopment(w, r)
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	body, err := decodeJSONBody(w, r, 256<<10)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var input struct {
		ExpectedRevision *int64   `json:"expected_revision"`
		DecisionID       string   `json:"decision_id"`
		CheckpointID     string   `json:"checkpoint_id"`
		Stage            string   `json:"stage"`
		ArtifactRef      string   `json:"artifact_ref"`
		Action           string   `json:"action"`
		Rationale        string   `json:"rationale,omitempty"`
		ExpectedTestIDs  []string `json:"expected_test_ids,omitempty"`
	}
	if decodeOneJSON(body, &input) != nil || input.ExpectedRevision == nil || *input.ExpectedRevision < 1 || !generationReviewIDPattern.MatchString(input.DecisionID) || (input.Stage != "plan" && input.Stage != "code") || (input.Action != "approve" && input.Action != "reject") || len(input.ExpectedTestIDs) > 1000 || strings.TrimSpace(input.Rationale) == "" || len(input.Rationale) > 2048 || strings.IndexFunc(input.Rationale, unicode.IsControl) >= 0 || !utf8.ValidString(input.Rationale) {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "invalid_generation_review")
		return
	}
	ids := slices.Clone(input.ExpectedTestIDs)
	for _, id := range ids {
		if len(id) > 1024 || !generationReviewTestIDPattern.MatchString(id) {
			writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "invalid_expected_test_id")
			return
		}
	}
	slices.Sort(ids)
	if len(slices.Compact(slices.Clone(ids))) != len(ids) {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "duplicate_expected_test_id")
		return
	}
	if input.Stage != "code" || input.Action != "approve" {
		if len(ids) != 0 {
			writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "test_inventory_requires_code_approval")
			return
		}
	} else if len(ids) == 0 {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "independent_test_inventory_required")
		return
	}
	run, err := a.Repository.GetRun(r.Context(), r.PathValue("runID"))
	if err != nil {
		if errors.Is(err, testgenflow.ErrRunNotFound) {
			writeTestGenerationDevelopmentError(w, http.StatusNotFound, "run_not_found")
		} else {
			writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "run_store_unavailable")
		}
		return
	}
	if run.Request.InputMode != testgenflow.InputModeModelGeneration || run.Request.Generation == nil || run.Request.ProfileDigest != a.Config.ProfileDigest || run.Request.Generation.PolicyDigest != a.Config.GenerationPolicyDigest {
		writeTestGenerationDevelopmentError(w, http.StatusConflict, "generation_policy_not_active")
		return
	}
	decision := testgenflow.GenerationReview{
		DecisionID: input.DecisionID, CheckpointID: input.CheckpointID, Stage: input.Stage, ArtifactRef: input.ArtifactRef,
		Action: input.Action, ReviewerID: a.Config.ReviewerID, ReviewerRole: a.Config.ReviewerRole, AuthorizationRef: a.Config.ReviewAuthorizationRef,
		Rationale: strings.TrimSpace(input.Rationale), ExpectedTestIDs: ids, DecidedAt: a.now().Truncate(time.Microsecond),
	}
	var previous *testgenflow.GenerationReview
	for i := range run.Reviews {
		if run.Reviews[i].DecisionID == input.DecisionID {
			previous = &run.Reviews[i]
			break
		}
	}
	if previous != nil {
		decision.Attempt = previous.Attempt
		decision.DecidedAt = previous.DecidedAt
		decision.ApprovedTestArtifactDigest = previous.ApprovedTestArtifactDigest
		if decision.CheckpointID != previous.CheckpointID || decision.Stage != previous.Stage || decision.ArtifactRef != previous.ArtifactRef || decision.Action != previous.Action || decision.ReviewerID != previous.ReviewerID || decision.ReviewerRole != previous.ReviewerRole || decision.AuthorizationRef != previous.AuthorizationRef || decision.Rationale != previous.Rationale || !slices.Equal(decision.ExpectedTestIDs, previous.ExpectedTestIDs) {
			writeTestGenerationDevelopmentError(w, http.StatusConflict, "review_decision_conflict")
			return
		}
	} else {
		if run.Checkpoint == nil || run.Status != testgenflow.StatusWaitingReview || run.Checkpoint.ID != input.CheckpointID || run.Checkpoint.Stage != input.Stage || run.Checkpoint.ArtifactRef != input.ArtifactRef {
			writeTestGenerationDevelopmentError(w, http.StatusConflict, "checkpoint_not_current")
			return
		}
		decision.Attempt = run.Checkpoint.Attempt
		if run.Revision != *input.ExpectedRevision || !decision.DecidedAt.Before(run.Checkpoint.ExpiresAt) {
			writeTestGenerationDevelopmentError(w, http.StatusConflict, "review_conflict_or_expired")
			return
		}
		if input.Stage == "code" && input.Action == "approve" {
			decision.ApprovedTestArtifactDigest, err = a.ApproveGeneratedCode(r.Context(), input.ArtifactRef, ids)
			if err != nil {
				writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "code_candidate_or_test_inventory_invalid")
				return
			}
		}
	}
	sum := sha256.Sum256([]byte(input.DecisionID))
	updated, err := a.Repository.ApplyEvent(r.Context(), run.RunID, testgenflow.Event{
		ID: "generation-review:" + hex.EncodeToString(sum[:]), ExpectedRevision: *input.ExpectedRevision,
		Type: testgenflow.EventGenerationReviewDecided, OccurredAt: decision.DecidedAt, Review: &decision,
	})
	if err != nil {
		if errors.Is(err, testgenflow.ErrRevisionConflict) || errors.Is(err, testgenflow.ErrEventConflict) || errors.Is(err, testgenflow.ErrInvalidEvent) || errors.Is(err, testgenflow.ErrInvalidTransition) {
			writeTestGenerationDevelopmentError(w, http.StatusConflict, "review_conflict_or_expired")
		} else {
			writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "review_persistence_unavailable")
		}
		return
	}
	writeTestGenerationDevelopmentResponse(w, http.StatusAccepted, updated)
}
