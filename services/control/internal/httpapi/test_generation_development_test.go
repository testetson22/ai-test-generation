package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

const testGenerationRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestDevelopmentAPIStartsPinnedRunAndProjectsLegacyFields(t *testing.T) {
	api, repository := newTestGenerationDevelopmentAPI()
	server := NewTestGenerationDevelopmentHandler(api)
	requestBody := `{"issue_iid":"42"}`
	response := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", requestBody, "trigger:42", api.Config.BearerToken)
	if response.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body %s", response.Code, response.Body.String())
	}
	var body TestGenerationDevelopmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if body.Run.Request.RepositoryURL != api.Config.RepositoryURL || body.Run.Request.Revision != api.Config.PinnedRevision || body.Run.Request.IssueIID != "42" {
		t.Fatalf("run accepted an unconfigured source: %#v", body.Run.Request)
	}
	if body.Run.Request.ProfileID != api.Config.ProfileID || body.Run.Request.ProfileVersion != api.Config.ProfileVersion || body.Run.Request.ProfileDigest != api.Config.ProfileDigest {
		t.Fatalf("run profile identity = %#v, want configured identity", body.Run.Request)
	}
	if body.PipelineResults.ChainsGitURL != api.Config.RepositoryURL || body.PipelineResults.ChainsGitCommit != "" ||
		body.PipelineResults.TestsAdded != "0" || body.PipelineResults.PRURL != "" || body.PipelineResults.RunRef != body.Run.RunID {
		t.Fatalf("initial pipeline results = %#v", body.PipelineResults)
	}
	if body.GenerationTaskResults.Attempts != "0" || body.GenerationTaskResults.LastError != "" {
		t.Fatalf("initial generation task results = %#v", body.GenerationTaskResults)
	}
	if response.Header().Get("Location") != "/development/test-generation-runs/"+body.Run.RunID {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}

	replayed := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", requestBody, "trigger:42", api.Config.BearerToken)
	if replayed.Code != http.StatusOK {
		t.Fatalf("idempotent start status = %d, body %s", replayed.Code, replayed.Body.String())
	}
	var replayedBody TestGenerationDevelopmentResponse
	if err := json.Unmarshal(replayed.Body.Bytes(), &replayedBody); err != nil || replayedBody.Run.RunID != body.Run.RunID {
		t.Fatalf("idempotent response = %#v, err %v", replayedBody, err)
	}
	if _, err := repository.GetRun(t.Context(), body.Run.RunID); err != nil {
		t.Fatalf("stored run not found: %v", err)
	}
}

func TestDevelopmentAPIRejectsChangedIdempotencyAndUnconfiguredSource(t *testing.T) {
	api, _ := newTestGenerationDevelopmentAPI()
	server := NewTestGenerationDevelopmentHandler(api)
	started := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", `{"issue_iid":"42"}`, "trigger:42", api.Config.BearerToken)
	if started.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body %s", started.Code, started.Body.String())
	}
	changed := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", `{"issue_iid":"42","base_branch":"release"}`, "trigger:42", api.Config.BearerToken)
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed idempotent request status = %d, body %s", changed.Code, changed.Body.String())
	}
	unknownSource := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", `{"repository_url":"https://attacker.example/repo.git","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "other-trigger", api.Config.BearerToken)
	if unknownSource.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured source fields status = %d, body %s", unknownSource.Code, unknownSource.Body.String())
	}
}

func TestDevelopmentAPIRequiresDevelopmentBearerToken(t *testing.T) {
	api, _ := newTestGenerationDevelopmentAPI()
	server := NewTestGenerationDevelopmentHandler(api)
	for _, token := range []string{"", "wrong-token"} {
		response := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", `{}`, "trigger:42", token)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("token %q status = %d, want 401", token, response.Code)
		}
	}
	unconfigured := httptest.NewRecorder()
	NewTestGenerationDevelopmentHandler(nil).ServeHTTP(unconfigured, httptest.NewRequest(http.MethodPost, "/development/test-generation-runs", nil))
	if unconfigured.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled development API status = %d, want 503", unconfigured.Code)
	}
}

func TestDevelopmentAPIDoesNotLetWorkerCallerMutateRunState(t *testing.T) {
	api, _ := newTestGenerationDevelopmentAPI()
	server := NewTestGenerationDevelopmentHandler(api)
	started := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", `{"issue_iid":"42"}`, "trigger:42", api.Config.BearerToken)
	var body TestGenerationDevelopmentResponse
	if err := json.Unmarshal(started.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	workerMutation := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs/"+body.Run.RunID+"/events", `{}`, "", api.Config.BearerToken)
	if workerMutation.Code != http.StatusNotFound {
		t.Fatalf("unexposed worker mutation status = %d, want 404", workerMutation.Code)
	}
	get := testGenerationRequest(server, http.MethodGet, "/development/test-generation-runs/"+body.Run.RunID, "", "", api.Config.BearerToken)
	if get.Code != http.StatusOK {
		t.Fatalf("get run status = %d, body %s", get.Code, get.Body.String())
	}
	var unchanged TestGenerationDevelopmentResponse
	if err := json.Unmarshal(get.Body.Bytes(), &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Run.Revision != body.Run.Revision || unchanged.Run.Status != testgenflow.StatusQueued {
		t.Fatalf("worker changed controller-owned state: %#v", unchanged.Run)
	}
}

func TestDevelopmentAPIRejectsWeakConfiguration(t *testing.T) {
	api, _ := newTestGenerationDevelopmentAPI()
	api.Config.BearerToken = "short"
	response := testGenerationRequest(NewTestGenerationDevelopmentHandler(api), http.MethodPost, "/development/test-generation-runs", `{}`, "trigger:42", "short")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("weak configuration status = %d, want 503", response.Code)
	}
	api, _ = newTestGenerationDevelopmentAPI()
	api.Config.ProfileDigest = ""
	response = testGenerationRequest(NewTestGenerationDevelopmentHandler(api), http.MethodPost, "/development/test-generation-runs", `{}`, "trigger:42", api.Config.BearerToken)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing Maven profile identity status = %d, want 503", response.Code)
	}
}

func newTestGenerationDevelopmentAPI() (*TestGenerationDevelopmentAPI, *memoryTestGenerationRepository) {
	now := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	repository := &memoryTestGenerationRepository{runs: make(map[string]testgenflow.Run), keys: make(map[string]string), events: make(map[string]map[string]string)}
	return &TestGenerationDevelopmentAPI{
		Repository: repository,
		Config: TestGenerationDevelopmentConfig{
			BearerToken:       strings.Repeat("d", 32),
			RepositoryURL:     "https://gitlab.example/group/spring-demo.git",
			PinnedRevision:    testGenerationRevision,
			DefaultBaseBranch: "main",
			Subdirectory:      "source",
			ProfileID:         "spring-java17-maven",
			ProfileVersion:    "0.1.0",
			ProfileDigest:     "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			MaxAttempts:       6,
			Now:               func() time.Time { return now },
		},
	}, repository
}

func testGenerationRequest(handler http.Handler, method, path, body, idempotencyKey, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type memoryTestGenerationRepository struct {
	mu     sync.Mutex
	runs   map[string]testgenflow.Run
	keys   map[string]string
	events map[string]map[string]string
}

func (r *memoryTestGenerationRepository) CreateRun(_ context.Context, run testgenflow.Run) (testgenflow.Run, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if runID, exists := r.keys[run.IdempotencyKey]; exists {
		existing := r.runs[runID]
		if existing.RequestDigest != run.RequestDigest {
			return testgenflow.Run{}, false, testgenflow.ErrIdempotencyConflict
		}
		return cloneTestGenerationRun(existing), false, nil
	}
	r.runs[run.RunID] = cloneTestGenerationRun(run)
	r.keys[run.IdempotencyKey] = run.RunID
	r.events[run.RunID] = make(map[string]string)
	return cloneTestGenerationRun(run), true, nil
}

func (r *memoryTestGenerationRepository) GetRun(_ context.Context, runID string) (testgenflow.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, exists := r.runs[runID]
	if !exists {
		return testgenflow.Run{}, testgenflow.ErrRunNotFound
	}
	return cloneTestGenerationRun(run), nil
}

func (r *memoryTestGenerationRepository) ApplyEvent(_ context.Context, runID string, event testgenflow.Event) (testgenflow.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, exists := r.runs[runID]
	if !exists {
		return testgenflow.Run{}, testgenflow.ErrRunNotFound
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return testgenflow.Run{}, err
	}
	digest := sha256.Sum256(encoded)
	eventDigest := hex.EncodeToString(digest[:])
	if prior, exists := r.events[runID][event.ID]; exists {
		if prior != eventDigest {
			return testgenflow.Run{}, testgenflow.ErrEventConflict
		}
		return cloneTestGenerationRun(run), nil
	}
	next, err := run.Apply(event)
	if err != nil {
		return testgenflow.Run{}, err
	}
	r.runs[runID] = cloneTestGenerationRun(next)
	r.events[runID][event.ID] = eventDigest
	return cloneTestGenerationRun(next), nil
}

func cloneTestGenerationRun(run testgenflow.Run) testgenflow.Run {
	encoded, _ := json.Marshal(run)
	var cloned testgenflow.Run
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

var _ testgenflow.Repository = (*memoryTestGenerationRepository)(nil)
