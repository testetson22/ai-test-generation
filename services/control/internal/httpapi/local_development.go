package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

const MaxDevelopmentArtifactBytes = 40 << 20

func (a *TestGenerationDevelopmentAPI) cancelRun(w http.ResponseWriter, r *http.Request) {
	if !a.Ready() {
		unavailableTestGenerationDevelopment(w, r)
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	key, err := requiredIdempotencyKey(r)
	if err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "invalid_idempotency_key")
		return
	}
	body, err := decodeJSONBody(w, r, 1024)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var input struct {
		ExpectedRevision *int64 `json:"expected_revision"`
	}
	if err := decodeOneJSON(body, &input); err != nil || input.ExpectedRevision == nil || *input.ExpectedRevision < 1 {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "expected_revision_required")
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
	if run.Request.ProfileDigest != a.Config.ProfileDigest {
		writeTestGenerationDevelopmentError(w, http.StatusConflict, "run_profile_not_active")
		return
	}
	occurredAt := a.now().Truncate(time.Microsecond)
	if run.CancelRequestedAt != nil {
		occurredAt = *run.CancelRequestedAt
	}
	digest := sha256.Sum256([]byte(key))
	run, err = a.Repository.ApplyEvent(r.Context(), run.RunID, testgenflow.Event{
		ID: "cancel:" + hex.EncodeToString(digest[:]), ExpectedRevision: *input.ExpectedRevision,
		Type: testgenflow.EventCancelRequested, OccurredAt: occurredAt,
	})
	if err != nil {
		switch {
		case errors.Is(err, testgenflow.ErrRunNotFound):
			writeTestGenerationDevelopmentError(w, http.StatusNotFound, "run_not_found")
		case errors.Is(err, testgenflow.ErrRevisionConflict), errors.Is(err, testgenflow.ErrInvalidTransition), errors.Is(err, testgenflow.ErrEventConflict):
			writeTestGenerationDevelopmentError(w, http.StatusConflict, "run_changed_or_not_cancellable")
		default:
			writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "run_store_unavailable")
		}
		return
	}
	writeTestGenerationDevelopmentResponse(w, http.StatusAccepted, run)
}

func (a *TestGenerationDevelopmentAPI) testArtifactLimit() int64 {
	if a.MaxTestArtifactBytes > 0 && a.MaxTestArtifactBytes <= 1<<20 {
		return a.MaxTestArtifactBytes
	}
	return 256 << 10
}

func (a *TestGenerationDevelopmentAPI) uploadTestArtifact(w http.ResponseWriter, r *http.Request) {
	if !a.Ready() {
		unavailableTestGenerationDevelopment(w, r)
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	body, err := decodeJSONBody(w, r, a.testArtifactLimit())
	if err != nil {
		writeBodyError(w, err)
		return
	}
	if err := a.ValidateTestArtifact(body); err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "invalid_test_artifact")
		return
	}
	ref, err := a.Artifacts.Put(r.Context(), body)
	if err == nil {
		_, err = a.Artifacts.Get(r.Context(), ref.Digest, a.testArtifactLimit())
	}
	if err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "artifact_retention_failed")
		return
	}
	w.Header().Set("Location", "/development/artifacts/"+ref.Digest)
	writeJSON(w, http.StatusCreated, map[string]string{"digest": ref.Digest, "mode": "artifact_input"})
}

func (a *TestGenerationDevelopmentAPI) getArtifact(w http.ResponseWriter, r *http.Request) {
	if !a.Ready() {
		unavailableTestGenerationDevelopment(w, r)
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	data, err := a.Artifacts.Get(r.Context(), r.PathValue("digest"), MaxDevelopmentArtifactBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, artifact.ErrInvalidDigest) {
			writeTestGenerationDevelopmentError(w, http.StatusNotFound, "artifact_not_found")
		} else {
			writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "artifact_unavailable")
		}
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", `"`+r.PathValue("digest")+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *TestGenerationDevelopmentAPI) getOperations(w http.ResponseWriter, r *http.Request) {
	if !a.Ready() {
		unavailableTestGenerationDevelopment(w, r)
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	runID := r.PathValue("runID")
	if _, err := a.Repository.GetRun(r.Context(), runID); err != nil {
		if errors.Is(err, testgenflow.ErrRunNotFound) {
			writeTestGenerationDevelopmentError(w, http.StatusNotFound, "run_not_found")
		} else {
			writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "run_store_unavailable")
		}
		return
	}
	ops, err := a.Operations(r.Context(), runID)
	if err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "operation_store_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "operations": ops})
}
