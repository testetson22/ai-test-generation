package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgensim"
)

const maxTestGenerationDevelopmentRequestBytes = 16 * 1024

type TestGenerationDevelopmentConfig struct {
	BearerToken            string
	RepositoryURL          string
	PinnedRevision         string
	DefaultBaseBranch      string
	Subdirectory           string
	ProfileID              string
	ProfileVersion         string
	ProfileDigest          string
	MaxAttempts            int
	InputMode              string
	CompletionMode         string
	GenerationPolicyDigest string
	GenerationContextPaths []string
	ReviewerID             string
	ReviewerRole           string
	ReviewAuthorizationRef string
	Now                    func() time.Time
}

type TestGenerationDevelopmentAPI struct {
	Repository           testgenflow.Repository
	Config               TestGenerationDevelopmentConfig
	Artifacts            artifact.Store
	ValidateTestArtifact func([]byte) error
	MaxTestArtifactBytes int64
	Operations           func(context.Context, string) ([]testgenflow.Operation, error)
	ApproveGeneratedCode func(context.Context, string, []string) (string, error)
}

type StartTestGenerationDevelopmentRequest struct {
	BaseBranch          string `json:"base_branch,omitempty"`
	IssueIID            string `json:"issue_iid,omitempty"`
	Subdirectory        string `json:"subdirectory,omitempty"`
	VerifyCommit        bool   `json:"verify_commit,omitempty"`
	InputArtifactDigest string `json:"input_artifact_digest,omitempty"`
	Objective           string `json:"objective,omitempty"`
}

type TestGenerationDevelopmentResponse struct {
	Run                   testgenflow.Run                   `json:"run"`
	PipelineResults       testgenflow.PipelineResults       `json:"pipeline_results"`
	GenerationTaskResults testgenflow.GenerationTaskResults `json:"generation_task_results"`
}

func NewTestGenerationDevelopmentHandler(api *TestGenerationDevelopmentAPI) http.Handler {
	mux := http.NewServeMux()
	if api == nil {
		mux.HandleFunc("POST /development/test-generation-runs", unavailableTestGenerationDevelopment)
		mux.HandleFunc("GET /development/test-generation-runs/{runID}", unavailableTestGenerationDevelopment)
		return mux
	}
	api.Register(mux)
	return mux
}

func NewTestGenerationSimulationHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /development/simulations/test-generation", simulateTestGeneration)
	return mux
}

func simulateTestGeneration(w http.ResponseWriter, r *http.Request) {
	body, err := decodeJSONBody(w, r, 1024)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var input struct {
		Scenario testgensim.Scenario `json:"scenario"`
	}
	if err := decodeOneJSON(body, &input); err != nil || !validTestGenerationScenario(input.Scenario) {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "invalid_simulation_request")
		return
	}
	report, err := testgensim.Run(r.Context(), input.Scenario)
	if err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusInternalServerError, "simulation_failed")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func validTestGenerationScenario(scenario testgensim.Scenario) bool {
	switch scenario {
	case testgensim.ScenarioSuccess, testgensim.ScenarioRetry, testgensim.ScenarioNoChanges, testgensim.ScenarioUnknownResult:
		return true
	default:
		return false
	}
}

func (a *TestGenerationDevelopmentAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /development/test-generation-runs", a.createRun)
	mux.HandleFunc("GET /development/test-generation-runs/{runID}", a.getRun)
	if a.Artifacts != nil && a.ValidateTestArtifact != nil {
		mux.HandleFunc("POST /development/test-generation-runs/{runID}/cancel", a.cancelRun)
		mux.HandleFunc("POST /development/test-artifacts", a.uploadTestArtifact)
		mux.HandleFunc("GET /development/artifacts/{digest}", a.getArtifact)
		if a.Operations != nil {
			mux.HandleFunc("GET /development/test-generation-runs/{runID}/operations", a.getOperations)
		}
		if a.Config.InputMode == testgenflow.InputModeModelGeneration && a.ApproveGeneratedCode != nil {
			mux.HandleFunc("POST /development/test-generation-runs/{runID}/reviews", a.reviewGeneration)
		}
	}
}

func (a *TestGenerationDevelopmentAPI) Ready() bool {
	if a == nil || a.Repository == nil || len(a.Config.BearerToken) < 24 || strings.ContainsAny(a.Config.BearerToken, "\x00\r\n") || a.Config.MaxAttempts < 1 || a.Config.MaxAttempts > testgenflow.DefaultMaxGenerationAttempts {
		return false
	}
	request := testgenflow.Request{
		RepositoryURL:  a.Config.RepositoryURL,
		Revision:       a.Config.PinnedRevision,
		BaseBranch:     a.Config.DefaultBaseBranch,
		Subdirectory:   a.Config.Subdirectory,
		ProfileID:      a.Config.ProfileID,
		ProfileVersion: a.Config.ProfileVersion,
		ProfileDigest:  a.Config.ProfileDigest,
		InputMode:      a.Config.InputMode,
		CompletionMode: a.Config.CompletionMode,
	}
	if a.Config.InputMode == testgenflow.InputModeArtifactInput {
		if a.Artifacts == nil || a.ValidateTestArtifact == nil {
			return false
		}
		request.InputArtifactDigest = "sha256:" + strings.Repeat("0", 64)
	}
	if a.Config.InputMode == testgenflow.InputModeModelGeneration {
		if a.Artifacts == nil || a.ValidateTestArtifact == nil || a.ApproveGeneratedCode == nil || !ValidDevelopmentReviewer(a.Config) {
			return false
		}
		request.Generation = &testgenflow.GenerationRequest{Objective: "configuration probe", ContextPaths: append([]string(nil), a.Config.GenerationContextPaths...), PolicyDigest: a.Config.GenerationPolicyDigest}
	}
	return request.Validate() == nil
}

func (a *TestGenerationDevelopmentAPI) createRun(w http.ResponseWriter, r *http.Request) {
	if !a.Ready() {
		writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "development_flow_unconfigured")
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	body, err := decodeJSONBody(w, r, maxTestGenerationDevelopmentRequestBytes)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var input StartTestGenerationDevelopmentRequest
	if err := decodeOneJSON(body, &input); err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	idempotencyKey, err := requiredIdempotencyKey(r)
	if err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "invalid_idempotency_key")
		return
	}
	baseBranch := input.BaseBranch
	if baseBranch == "" {
		baseBranch = a.Config.DefaultBaseBranch
	}
	subdirectory := input.Subdirectory
	if subdirectory == "" {
		subdirectory = a.Config.Subdirectory
	}
	request := testgenflow.Request{
		RepositoryURL:       a.Config.RepositoryURL,
		Revision:            a.Config.PinnedRevision,
		BaseBranch:          baseBranch,
		IssueIID:            input.IssueIID,
		Subdirectory:        subdirectory,
		ProfileID:           a.Config.ProfileID,
		ProfileVersion:      a.Config.ProfileVersion,
		ProfileDigest:       a.Config.ProfileDigest,
		VerifyCommit:        input.VerifyCommit,
		InputMode:           a.Config.InputMode,
		CompletionMode:      a.Config.CompletionMode,
		InputArtifactDigest: input.InputArtifactDigest,
	}
	if a.Config.InputMode == testgenflow.InputModeModelGeneration {
		if input.InputArtifactDigest != "" || input.VerifyCommit || baseBranch != a.Config.DefaultBaseBranch || subdirectory != a.Config.Subdirectory {
			writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "unsupported_model_generation_options")
			return
		}
		request.Generation = &testgenflow.GenerationRequest{Objective: input.Objective, ContextPaths: append([]string(nil), a.Config.GenerationContextPaths...), PolicyDigest: a.Config.GenerationPolicyDigest}
	} else if input.Objective != "" {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "objective_requires_model_generation_mode")
		return
	}
	if a.Config.InputMode == testgenflow.InputModeArtifactInput {
		if input.VerifyCommit || baseBranch != a.Config.DefaultBaseBranch || subdirectory != a.Config.Subdirectory {
			writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "unsupported_local_execution_options")
			return
		}
		data, err := a.Artifacts.Get(r.Context(), input.InputArtifactDigest, a.testArtifactLimit())
		if err != nil || a.ValidateTestArtifact(data) != nil {
			writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "invalid_input_artifact")
			return
		}
	}
	runID, err := testgenflow.NewRunID()
	if err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "run_id_unavailable")
		return
	}
	createdAt := a.now()
	run, err := testgenflow.NewRun(runID, idempotencyKey, request, a.Config.MaxAttempts, createdAt)
	if err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	stored, created, err := a.Repository.CreateRun(r.Context(), run)
	if err != nil {
		if errors.Is(err, testgenflow.ErrIdempotencyConflict) {
			writeTestGenerationDevelopmentError(w, http.StatusConflict, "idempotency_conflict")
			return
		}
		writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "run_store_unavailable")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", "/development/test-generation-runs/"+stored.RunID)
	}
	writeTestGenerationDevelopmentResponse(w, status, stored)
}

func (a *TestGenerationDevelopmentAPI) getRun(w http.ResponseWriter, r *http.Request) {
	if !a.Ready() {
		writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "development_flow_unconfigured")
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	run, err := a.Repository.GetRun(r.Context(), r.PathValue("runID"))
	if errors.Is(err, testgenflow.ErrRunNotFound) {
		writeTestGenerationDevelopmentError(w, http.StatusNotFound, "run_not_found")
		return
	}
	if err != nil {
		writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "run_store_unavailable")
		return
	}
	writeTestGenerationDevelopmentResponse(w, http.StatusOK, run)
}

func (a *TestGenerationDevelopmentAPI) authenticate(w http.ResponseWriter, r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		writeTestGenerationDevelopmentError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if len(token) != len(a.Config.BearerToken) || subtle.ConstantTimeCompare([]byte(token), []byte(a.Config.BearerToken)) != 1 {
		writeTestGenerationDevelopmentError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (a *TestGenerationDevelopmentAPI) now() time.Time {
	if a.Config.Now != nil {
		return a.Config.Now().UTC()
	}
	return time.Now().UTC()
}

func writeTestGenerationDevelopmentResponse(w http.ResponseWriter, status int, run testgenflow.Run) {
	pipelineResults, generationTaskResults := run.CompatibilityResults()
	writeJSON(w, status, TestGenerationDevelopmentResponse{
		Run:                   run,
		PipelineResults:       pipelineResults,
		GenerationTaskResults: generationTaskResults,
	})
}

func writeTestGenerationDevelopmentError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func unavailableTestGenerationDevelopment(w http.ResponseWriter, _ *http.Request) {
	writeTestGenerationDevelopmentError(w, http.StatusServiceUnavailable, "development_flow_unconfigured")
}
