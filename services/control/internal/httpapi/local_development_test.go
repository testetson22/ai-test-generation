package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

const localTestArtifactJSON = `{
  "schema_version": "1",
  "files": [{"path":"src/test/java/example/UploadedTest.java","content":"package example; class UploadedTest {}\n"}],
  "expected_test_ids": ["example.UploadedTest#checks"]
}`

var changedLocalTestArtifactJSON = strings.Replace(localTestArtifactJSON, "class UploadedTest {}", "class UploadedTest { int value = 1; }", 1)

func TestLocalDevelopmentArtifactUploadAndDownloadPreserveExactBytes(t *testing.T) {
	api, repository, store := newLocalDevelopmentTestAPI(t)
	server := NewTestGenerationDevelopmentHandler(api)
	upload := testGenerationRequest(server, http.MethodPost, "/development/test-artifacts", localTestArtifactJSON, "", api.Config.BearerToken)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body %s", upload.Code, upload.Body.String())
	}
	var body struct {
		Digest string `json:"digest"`
		Mode   string `json:"mode"`
	}
	if err := json.Unmarshal(upload.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	digest := artifact.Digest([]byte(localTestArtifactJSON))
	location := "/development/artifacts/" + digest
	if body.Digest != digest || body.Mode != testgenflow.InputModeArtifactInput || upload.Header().Get("Location") != location || upload.Header().Get("Content-Type") != "application/json" || upload.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("upload identity/headers = %#v, %v", body, upload.Header())
	}
	retained, err := store.Get(t.Context(), digest, api.MaxTestArtifactBytes)
	if err != nil || !bytes.Equal(retained, []byte(localTestArtifactJSON)) {
		t.Fatalf("retained upload changed: %q, %v", retained, err)
	}
	download := testGenerationRequest(server, http.MethodGet, location, "", "", api.Config.BearerToken)
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), []byte(localTestArtifactJSON)) {
		t.Fatalf("download = %d, %q", download.Code, download.Body.Bytes())
	}
	for header, want := range map[string]string{"Content-Type": "application/octet-stream", "X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "ETag": `"` + digest + `"`} {
		if got := download.Header().Get(header); got != want {
			t.Errorf("download %s = %q, want %q", header, got, want)
		}
	}
	if len(repository.runs) != 0 || len(repository.keys) != 0 {
		t.Fatalf("artifact upload admitted a run: %#v", repository.runs)
	}
}

func TestLocalDevelopmentArtifactRoutesRequireAuthentication(t *testing.T) {
	api, repository, store := newLocalDevelopmentTestAPI(t)
	ref, err := store.Put(t.Context(), []byte(localTestArtifactJSON))
	if err != nil {
		t.Fatal(err)
	}
	validationCalls := 0
	validate := api.ValidateTestArtifact
	api.ValidateTestArtifact = func(body []byte) error { validationCalls++; return validate(body) }
	server := NewTestGenerationDevelopmentHandler(api)
	for _, token := range []string{"", "wrong-development-token"} {
		for _, route := range []struct{ method, path, body string }{
			{http.MethodPost, "/development/test-artifacts", changedLocalTestArtifactJSON},
			{http.MethodGet, "/development/artifacts/" + ref.Digest, ""},
		} {
			t.Run(route.method+"/"+token, func(t *testing.T) {
				response := testGenerationRequest(server, route.method, route.path, route.body, "", token)
				if response.Code != http.StatusUnauthorized || response.Header().Get("Location") != "" || bytes.Contains(response.Body.Bytes(), []byte("UploadedTest")) {
					t.Fatalf("unauthenticated artifact request = %d, %s, %v", response.Code, response.Body.String(), response.Header())
				}
			})
		}
	}
	if validationCalls != 0 || len(repository.runs) != 0 {
		t.Fatalf("unauthenticated upload reached validation/admission: calls=%d runs=%v", validationCalls, repository.runs)
	}
	if _, err := store.Get(t.Context(), artifact.Digest([]byte(changedLocalTestArtifactJSON)), 1<<20); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unauthenticated upload retained bytes: %v", err)
	}
}

func TestLocalDevelopmentArtifactUploadRejectsInvalidBodiesBeforeRetention(t *testing.T) {
	for _, test := range []struct {
		name, body, contentType string
		status                  int
		mustNotValidate         bool
	}{
		{name: "invalid JSON", body: `{"schema_version":`, contentType: "application/json", status: http.StatusBadRequest},
		{name: "duplicate field", body: `{"schema_version":"1","schema_version":"1","files":[],"expected_test_ids":[]}`, contentType: "application/json", status: http.StatusBadRequest},
		{name: "nested duplicate field", body: `{"schema_version":"1","files":[{"path":"src/test/java/A.java","path":"src/test/java/B.java","content":"x"}],"expected_test_ids":["A#test"]}`, contentType: "application/json", status: http.StatusBadRequest},
		{name: "multiple JSON values", body: localTestArtifactJSON + `{}`, contentType: "application/json", status: http.StatusBadRequest},
		{name: "malformed artifact", body: `{"schema_version":"1","files":[],"expected_test_ids":[]}`, contentType: "application/json", status: http.StatusBadRequest},
		{name: "missing content type", body: localTestArtifactJSON, status: http.StatusBadRequest, mustNotValidate: true},
		{name: "wrong content type", body: localTestArtifactJSON, contentType: "text/plain", status: http.StatusBadRequest, mustNotValidate: true},
		{name: "oversize", body: strings.Repeat(" ", 1025) + localTestArtifactJSON, contentType: "application/json", status: http.StatusRequestEntityTooLarge, mustNotValidate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, repository, store := newLocalDevelopmentTestAPI(t)
			validationCalls := 0
			validate := api.ValidateTestArtifact
			api.ValidateTestArtifact = func(body []byte) error { validationCalls++; return validate(body) }
			request := httptest.NewRequest(http.MethodPost, "/development/test-artifacts", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+api.Config.BearerToken)
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			NewTestGenerationDevelopmentHandler(api).ServeHTTP(response, request)
			if response.Code != test.status || response.Header().Get("Location") != "" {
				t.Fatalf("invalid upload = %d, body %s, headers=%v", response.Code, response.Body.String(), response.Header())
			}
			if test.mustNotValidate && validationCalls != 0 {
				t.Fatalf("rejected HTTP envelope reached artifact validator %d times", validationCalls)
			}
			if len(repository.runs) != 0 || len(repository.keys) != 0 || len(repository.events) != 0 {
				t.Fatalf("invalid upload admitted run state: %#v", repository)
			}
			if _, err := store.Get(t.Context(), artifact.Digest([]byte(test.body)), 1<<20); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid upload retained bytes: %v", err)
			}
		})
	}
}

func TestLocalDevelopmentArtifactDownloadRejectsUnknownAndMalformedDigests(t *testing.T) {
	api, _, _ := newLocalDevelopmentTestAPI(t)
	server := NewTestGenerationDevelopmentHandler(api)
	for _, digest := range []string{"not-a-digest", "sha256:" + strings.Repeat("e", 64)} {
		response := testGenerationRequest(server, http.MethodGet, "/development/artifacts/"+digest, "", "", api.Config.BearerToken)
		if response.Code != http.StatusNotFound {
			t.Fatalf("unknown digest %q status = %d, body %s", digest, response.Code, response.Body.String())
		}
	}
}

func TestLocalDevelopmentStartRequiresStoredValidatedArtifactAndFixedOptions(t *testing.T) {
	for _, test := range []struct{ name, extra, artifactBody, digest string }{
		{name: "missing digest", digest: ""},
		{name: "malformed digest", digest: "artifact://unbound"},
		{name: "unknown digest", digest: "sha256:" + strings.Repeat("e", 64)},
		{name: "stored invalid artifact", artifactBody: `{"schema_version":"1","files":[],"expected_test_ids":[]}`},
		{name: "unsupported subdirectory", artifactBody: localTestArtifactJSON, extra: `,"subdirectory":"other"`},
		{name: "unsupported base branch", artifactBody: localTestArtifactJSON, extra: `,"base_branch":"release"`},
		{name: "unsupported verification", artifactBody: localTestArtifactJSON, extra: `,"verify_commit":true`},
		{name: "caller generation mode", artifactBody: localTestArtifactJSON, extra: `,"input_mode":"generation"`},
		{name: "caller PR mode", artifactBody: localTestArtifactJSON, extra: `,"completion_mode":"pull_request"`},
		{name: "caller source", artifactBody: localTestArtifactJSON, extra: `,"repository_url":"https://other.example/repo.git"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, repository, store := newLocalDevelopmentTestAPI(t)
			digest := test.digest
			if test.artifactBody != "" {
				ref, err := store.Put(t.Context(), []byte(test.artifactBody))
				if err != nil {
					t.Fatal(err)
				}
				digest = ref.Digest
			}
			body := fmt.Sprintf(`{"input_artifact_digest":%q%s}`, digest, test.extra)
			response := testGenerationRequest(NewTestGenerationDevelopmentHandler(api), http.MethodPost, "/development/test-generation-runs", body, "local-start", api.Config.BearerToken)
			if response.Code != http.StatusBadRequest || len(repository.runs) != 0 || len(repository.keys) != 0 || len(repository.events) != 0 {
				t.Fatalf("invalid local start admitted state: status=%d body=%s runs=%v", response.Code, response.Body.String(), repository.runs)
			}
		})
	}
}

func TestLocalDevelopmentStartIsArtifactBoundIdempotentAndExecutionOnly(t *testing.T) {
	api, repository, store := newLocalDevelopmentTestAPI(t)
	now := api.Config.Now().Add(123456789 * time.Nanosecond)
	api.Config.Now = func() time.Time { return now }
	ref, err := store.Put(t.Context(), []byte(localTestArtifactJSON))
	if err != nil {
		t.Fatal(err)
	}
	server := NewTestGenerationDevelopmentHandler(api)
	requestBody := fmt.Sprintf(`{"input_artifact_digest":%q,"issue_iid":"42","base_branch":"main","subdirectory":"source","verify_commit":false}`, ref.Digest)
	response := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", requestBody, "local-trigger:42", api.Config.BearerToken)
	if response.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body %s", response.Code, response.Body.String())
	}
	var started TestGenerationDevelopmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	run := started.Run
	if run.Request.InputMode != testgenflow.InputModeArtifactInput || run.Request.CompletionMode != testgenflow.CompletionModePatchOnly || run.Request.InputArtifactDigest != ref.Digest || run.Request.VerifyCommit || run.Request.RepositoryURL != api.Config.RepositoryURL || run.Request.Revision != api.Config.PinnedRevision || run.Status != testgenflow.StatusQueued || run.Stage != testgenflow.StageSource || run.AttemptCount() != 0 || run.Candidate != nil {
		t.Fatalf("local start modes/source/state = %#v", run)
	}
	if run.PRURL != "" || run.ResultArtifactRef != "" || started.PipelineResults.PRURL != "" || started.PipelineResults.ResultArtifactRef != "" || started.GenerationTaskResults.Attempts != "0" || started.PipelineResults.TerminalStatus != string(testgenflow.StatusQueued) {
		t.Fatalf("local start invented generation/PR/publication results: %#v", started)
	}
	if response.Header().Get("Location") != "/development/test-generation-runs/"+run.RunID {
		t.Fatalf("local run Location = %q", response.Header().Get("Location"))
	}
	now = now.Add(time.Hour)
	replayed := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", requestBody, "local-trigger:42", api.Config.BearerToken)
	var replayedBody TestGenerationDevelopmentResponse
	if err := json.Unmarshal(replayed.Body.Bytes(), &replayedBody); err != nil || replayed.Code != http.StatusOK || !reflect.DeepEqual(replayedBody, started) {
		t.Fatalf("idempotent artifact start = %d, %#v, err=%v", replayed.Code, replayedBody, err)
	}
	changed, err := store.Put(t.Context(), []byte(changedLocalTestArtifactJSON))
	if err != nil {
		t.Fatal(err)
	}
	conflictingBody := strings.Replace(requestBody, ref.Digest, changed.Digest, 1)
	conflict := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", conflictingBody, "local-trigger:42", api.Config.BearerToken)
	if conflict.Code != http.StatusConflict || len(repository.runs) != 1 || len(repository.keys) != 1 || len(repository.events[run.RunID]) != 0 {
		t.Fatalf("changed artifact reused key: status=%d body=%s runs=%v", conflict.Code, conflict.Body.String(), repository.runs)
	}
	stored, err := repository.GetRun(t.Context(), run.RunID)
	if err != nil || !reflect.DeepEqual(stored, run) {
		t.Fatalf("idempotency conflict changed admitted run: %#v, %v", stored, err)
	}
}

func TestLocalDevelopmentCancellationChecksRevisionAndReplaysOriginalTimestamp(t *testing.T) {
	api, repository, store := newLocalDevelopmentTestAPI(t)
	now := api.Config.Now()
	api.Config.Now = func() time.Time { return now }
	ref, err := store.Put(t.Context(), []byte(localTestArtifactJSON))
	if err != nil {
		t.Fatal(err)
	}
	server := NewTestGenerationDevelopmentHandler(api)
	start := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", fmt.Sprintf(`{"input_artifact_digest":%q}`, ref.Digest), "local-cancel-start", api.Config.BearerToken)
	var started TestGenerationDevelopmentResponse
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil || start.Code != http.StatusCreated {
		t.Fatalf("start = %d, %s, %v", start.Code, start.Body.String(), err)
	}
	path := "/development/test-generation-runs/" + started.Run.RunID + "/cancel"
	stale := testGenerationRequest(server, http.MethodPost, path, `{"expected_revision":2}`, "stale-cancel", api.Config.BearerToken)
	if stale.Code != http.StatusConflict || len(repository.events[started.Run.RunID]) != 0 {
		t.Fatalf("stale cancellation changed state: %d, %s", stale.Code, stale.Body.String())
	}
	unchanged, err := repository.GetRun(t.Context(), started.Run.RunID)
	if err != nil || !reflect.DeepEqual(unchanged, started.Run) {
		t.Fatalf("stale cancellation changed run: %#v, %v", unchanged, err)
	}
	now = now.Add(time.Minute)
	requested := testGenerationRequest(server, http.MethodPost, path, `{"expected_revision":1}`, "local-cancel", api.Config.BearerToken)
	var cancelled TestGenerationDevelopmentResponse
	if err := json.Unmarshal(requested.Body.Bytes(), &cancelled); err != nil || requested.Code != http.StatusAccepted {
		t.Fatalf("cancel request = %d, %s, %v", requested.Code, requested.Body.String(), err)
	}
	if cancelled.Run.Status != testgenflow.StatusCancelRequested || cancelled.Run.Revision != 2 || cancelled.Run.CancelRequestedAt == nil || !cancelled.Run.CancelRequestedAt.Equal(now.Truncate(time.Microsecond)) || cancelled.Run.PRURL != "" || cancelled.Run.ResultArtifactRef != "" || cancelled.PipelineResults.TerminalStatus != string(testgenflow.StatusCancelRequested) {
		t.Fatalf("cancellation claimed terminal success or lost revision/time: %#v", cancelled)
	}
	now = now.Add(24 * time.Hour)
	replayed := testGenerationRequest(server, http.MethodPost, path, `{"expected_revision":1}`, "local-cancel", api.Config.BearerToken)
	var replayedBody TestGenerationDevelopmentResponse
	if err := json.Unmarshal(replayed.Body.Bytes(), &replayedBody); err != nil || replayed.Code != http.StatusAccepted || !reflect.DeepEqual(replayedBody, cancelled) || len(repository.events[started.Run.RunID]) != 1 {
		t.Fatalf("same-key old-revision cancellation was not idempotent across clock advance: %d, %s, %v", replayed.Code, replayed.Body.String(), err)
	}
	conflict := testGenerationRequest(server, http.MethodPost, path, `{"expected_revision":2}`, "local-cancel", api.Config.BearerToken)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("different payload reused cancel key: %d, %s", conflict.Code, conflict.Body.String())
	}
	stored, err := repository.GetRun(t.Context(), started.Run.RunID)
	if err != nil || !reflect.DeepEqual(stored, cancelled.Run) || len(repository.events[started.Run.RunID]) != 1 {
		t.Fatalf("cancel replay/conflict mutated run: %#v, %v", stored, err)
	}
	terminal, err := repository.ApplyEvent(t.Context(), stored.RunID, testgenflow.Event{ID: "worker-cancelled", ExpectedRevision: stored.Revision, Type: testgenflow.EventCancelled, OccurredAt: now.Truncate(time.Microsecond), TerminationConfirmed: true})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	terminalReplay := testGenerationRequest(server, http.MethodPost, path, `{"expected_revision":1}`, "local-cancel", api.Config.BearerToken)
	var afterTerminal TestGenerationDevelopmentResponse
	if err := json.Unmarshal(terminalReplay.Body.Bytes(), &afterTerminal); err != nil || terminalReplay.Code != http.StatusAccepted || !reflect.DeepEqual(afterTerminal.Run, terminal) || len(repository.events[started.Run.RunID]) != 2 {
		t.Fatalf("cancel replay after termination: %d %s %v", terminalReplay.Code, terminalReplay.Body.String(), err)
	}
}

func TestLocalDevelopmentCancellationRequiresAuthenticationRevisionAndKey(t *testing.T) {
	for _, test := range []struct {
		name, body, key, token string
		status                 int
	}{
		{name: "unauthenticated", body: `{"expected_revision":1}`, key: "cancel", status: http.StatusUnauthorized},
		{name: "missing key", body: `{"expected_revision":1}`, token: strings.Repeat("d", 32), status: http.StatusBadRequest},
		{name: "missing revision", body: `{}`, key: "cancel", token: strings.Repeat("d", 32), status: http.StatusBadRequest},
		{name: "zero revision", body: `{"expected_revision":0}`, key: "cancel", token: strings.Repeat("d", 32), status: http.StatusBadRequest},
		{name: "fractional revision", body: `{"expected_revision":1.5}`, key: "cancel", token: strings.Repeat("d", 32), status: http.StatusBadRequest},
		{name: "duplicate revision", body: `{"expected_revision":1,"expected_revision":2}`, key: "cancel", token: strings.Repeat("d", 32), status: http.StatusBadRequest},
		{name: "unknown run", body: `{"expected_revision":1}`, key: "cancel", token: strings.Repeat("d", 32), status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, repository, _ := newLocalDevelopmentTestAPI(t)
			response := testGenerationRequest(NewTestGenerationDevelopmentHandler(api), http.MethodPost, "/development/test-generation-runs/missing-run/cancel", test.body, test.key, test.token)
			if response.Code != test.status || len(repository.runs) != 0 || len(repository.events) != 0 {
				t.Fatalf("invalid cancellation = %d, %s, state=%v", response.Code, response.Body.String(), repository.runs)
			}
		})
	}
}

func TestLocalCancellationRejectsInactiveProfileWithoutQueuingWork(t *testing.T) {
	api, repository, store := newLocalDevelopmentTestAPI(t)
	ref, err := store.Put(t.Context(), []byte(localTestArtifactJSON))
	if err != nil {
		t.Fatal(err)
	}
	server := NewTestGenerationDevelopmentHandler(api)
	created := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs", fmt.Sprintf(`{"input_artifact_digest":%q}`, ref.Digest), "profile-bound-run", api.Config.BearerToken)
	var body TestGenerationDevelopmentResponse
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil || created.Code != http.StatusCreated {
		t.Fatalf("create: %s %v", created.Body.String(), err)
	}
	api.Config.ProfileDigest = "sha256:" + strings.Repeat("a", 64)
	response := testGenerationRequest(server, http.MethodPost, "/development/test-generation-runs/"+body.Run.RunID+"/cancel", `{"expected_revision":1}`, "inactive-profile-cancel", api.Config.BearerToken)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "run_profile_not_active") || len(repository.events[body.Run.RunID]) != 0 {
		t.Fatalf("inactive-profile cancellation: %d %s", response.Code, response.Body.String())
	}
}

func TestLegacyDevelopmentHandlerDoesNotRegisterLocalArtifactOrCancellationRoutes(t *testing.T) {
	api, repository := newTestGenerationDevelopmentAPI()
	server := NewTestGenerationDevelopmentHandler(api)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/development/test-artifacts", localTestArtifactJSON},
		{http.MethodGet, "/development/artifacts/sha256:" + strings.Repeat("a", 64), ""},
		{http.MethodPost, "/development/test-generation-runs/legacy-run/cancel", `{"expected_revision":1}`},
	} {
		response := testGenerationRequest(server, route.method, route.path, route.body, "legacy-key", api.Config.BearerToken)
		if response.Code != http.StatusNotFound {
			t.Errorf("legacy handler exposed %s %s: %d, %s", route.method, route.path, response.Code, response.Body.String())
		}
	}
	if len(repository.runs) != 0 || len(repository.events) != 0 {
		t.Fatalf("legacy local-only routes changed state: %#v", repository)
	}
}

func newLocalDevelopmentTestAPI(t *testing.T) (*TestGenerationDevelopmentAPI, *memoryTestGenerationRepository, *artifact.FileStore) {
	t.Helper()
	api, repository := newTestGenerationDevelopmentAPI()
	store, err := artifact.NewFileStore(t.TempDir(), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close test artifact store: %v", err)
		}
	})
	api.Config.InputMode = testgenflow.InputModeArtifactInput
	api.Config.CompletionMode = testgenflow.CompletionModePatchOnly
	api.Artifacts = store
	api.MaxTestArtifactBytes = 1024
	api.ValidateTestArtifact = func(body []byte) error {
		if bytes.Equal(body, []byte(localTestArtifactJSON)) || bytes.Equal(body, []byte(changedLocalTestArtifactJSON)) {
			return nil
		}
		return errors.New("artifact is not a recognized test fixture")
	}
	return api, repository, store
}
