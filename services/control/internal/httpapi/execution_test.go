package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
)

func TestCreatePairResolvesAuthorizedReferencesAndIsIdempotent(t *testing.T) {
	handler, repository, authenticator, authorizer, resolver, audit, now := newExecutionHandler()
	request := validCreateRequest()
	body := marshalJSON(t, request)
	first := sendJSON(handler, http.MethodPost, "/execution-pairs", body, "request-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, body %s", first.Code, first.Body)
	}
	if got := first.Header().Get("Location"); got != "/execution-pairs/pair-1" {
		t.Fatalf("Location = %q", got)
	}
	var response PairResponse
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode pair response: %v", err)
	}
	if response.Contract != "atg.execution_pair_view" || response.Version != "1.0" || response.PairID != "pair-1" ||
		response.BaseTask.Contract != "atg.execution_task_view" || response.BaseTask.TaskID != "task-base" ||
		response.CandidateTask.Contract != "atg.execution_task_view" || response.CandidateTask.TaskID != "task-candidate" {
		t.Fatalf("pair response = %#v", response)
	}
	if response.BaseTask.Specification.Digest == "" || strings.Contains(first.Body.String(), "expected_test_ids") {
		t.Fatalf("response omitted task digest or exposed task specification: %s", first.Body)
	}
	if repository.createCalls != 1 || resolver.calls != 1 || authenticator.calls != 1 {
		t.Fatalf("create/auth/resolve calls = %d/%d/%d", repository.createCalls, authenticator.calls, resolver.calls)
	}
	if authorizer.permission != PermissionCreatePair || authorizer.resource != request.RunID {
		t.Fatalf("authorization = (%q, %q)", authorizer.permission, authorizer.resource)
	}
	if audit.calls != 1 || audit.last.IdempotencyKey != "request-1" || audit.last.RequestDigest != digestBytes(body) || !audit.last.AuthorizedAt.Equal(now) {
		t.Fatalf("audit event = %#v, calls %d", audit.last, audit.calls)
	}
	if repository.lastInput.IdempotencyKey != "request-1" || !repository.lastInput.CreatedAt.Equal(now) {
		t.Fatalf("persisted pair input = %#v", repository.lastInput)
	}

	duplicate := sendJSON(handler, http.MethodPost, "/execution-pairs", body, "request-1")
	if duplicate.Code != http.StatusOK || repository.createCalls != 2 {
		t.Fatalf("duplicate create status/calls = %d/%d, body %s", duplicate.Code, repository.createCalls, duplicate.Body)
	}
}

func TestCreatePairFailsClosedOnAuthenticationAuthorizationAndAudit(t *testing.T) {
	tests := []struct {
		name       string
		authErr    error
		authorErr  error
		auditErr   error
		wantStatus int
	}{
		{name: "unauthenticated", authErr: ErrUnauthenticated, wantStatus: http.StatusUnauthorized},
		{name: "forbidden", authorErr: ErrForbidden, wantStatus: http.StatusForbidden},
		{name: "audit unavailable", auditErr: errors.New("audit store down"), wantStatus: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, repository, authenticator, authorizer, resolver, audit, _ := newExecutionHandler()
			authenticator.err = test.authErr
			authorizer.err = test.authorErr
			audit.err = test.auditErr
			response := sendJSON(handler, http.MethodPost, "/execution-pairs", marshalJSON(t, validCreateRequest()), "request-1")
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body)
			}
			if repository.createCalls != 0 || resolver.calls != 0 {
				t.Fatalf("unauthorized/un-audited create reached backend: creates=%d resolves=%d", repository.createCalls, resolver.calls)
			}
		})
	}
}

func TestCreatePairRejectsInvalidAndOversizedRequests(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		mediaType  string
		key        string
		wantStatus int
	}{
		{name: "unknown field", content: `{"contract":"atg.execution_pair_request","version":"1.0","run_id":"run-1","expected_run_revision":3,"generation_id":"gen-1","profile_id":"profile-1","profile_version":"1.0.0","selection_id":"selection-1","comparison_mode":"same_subject_control","task_spec":{}}`, mediaType: "application/json", key: "request-1", wantStatus: http.StatusBadRequest},
		{name: "duplicate field", content: `{"contract":"atg.execution_pair_request","version":"1.0","run_id":"run-1","run_id":"run-2","expected_run_revision":3,"generation_id":"gen-1","profile_id":"profile-1","profile_version":"1.0.0","selection_id":"selection-1","comparison_mode":"same_subject_control"}`, mediaType: "application/json", key: "request-1", wantStatus: http.StatusBadRequest},
		{name: "missing idempotency key", content: string(marshalJSON(t, validCreateRequest())), mediaType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "wrong content type", content: "{}", mediaType: "text/plain", key: "request-1", wantStatus: http.StatusBadRequest},
		{name: "oversized body", content: strings.Repeat(" ", maxExecutionRequestBytes+1), mediaType: "application/json", key: "request-1", wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, repository, _, _, _, _, _ := newExecutionHandler()
			req := httptest.NewRequest(http.MethodPost, "/execution-pairs", strings.NewReader(test.content))
			req.Header.Set("Content-Type", test.mediaType)
			if test.key != "" {
				req.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body)
			}
			if repository.createCalls != 0 {
				t.Fatalf("invalid request reached storage %d times", repository.createCalls)
			}
		})
	}
}

func TestResolvedPairMustMatchRequestedReferences(t *testing.T) {
	handler, repository, _, _, resolver, _, _ := newExecutionHandler()
	resolver.mutate = func(input *execution.PairInput) {
		base, _ := execution.DecodeTaskSpec(input.BaseSpec)
		base.GenerationID = "forged-generation"
		input.BaseSpec, _ = json.Marshal(base)
	}
	response := sendJSON(handler, http.MethodPost, "/execution-pairs", marshalJSON(t, validCreateRequest()), "request-1")
	if response.Code != http.StatusUnprocessableEntity || repository.createCalls != 0 {
		t.Fatalf("status/create calls = %d/%d, body %s", response.Code, repository.createCalls, response.Body)
	}
}

func TestCreatePairMapsIdempotencyConflictAndBackendFailure(t *testing.T) {
	t.Run("different resolved bytes conflict", func(t *testing.T) {
		handler, _, _, _, resolver, _, _ := newExecutionHandler()
		request := validCreateRequest()
		body := marshalJSON(t, request)
		first := sendJSON(handler, http.MethodPost, "/execution-pairs", body, "request-1")
		if first.Code != http.StatusCreated {
			t.Fatalf("first create = %d: %s", first.Code, first.Body)
		}
		resolver.mutate = func(input *execution.PairInput) {
			base, _ := execution.DecodeTaskSpec(input.BaseSpec)
			candidate, _ := execution.DecodeTaskSpec(input.CandidateSpec)
			base.ExpectedTestIDs = []string{"sample.OtherTest#works"}
			candidate.ExpectedTestIDs = []string{"sample.OtherTest#works"}
			input.BaseSpec, _ = json.Marshal(base)
			input.CandidateSpec, _ = json.Marshal(candidate)
		}
		conflict := sendJSON(handler, http.MethodPost, "/execution-pairs", body, "request-1")
		if conflict.Code != http.StatusConflict {
			t.Fatalf("conflicting create = %d: %s", conflict.Code, conflict.Body)
		}
	})

	t.Run("storage unavailable", func(t *testing.T) {
		handler, repository, _, _, _, _, _ := newExecutionHandler()
		repository.createErr = errors.New("database password or internal detail")
		response := sendJSON(handler, http.MethodPost, "/execution-pairs", marshalJSON(t, validCreateRequest()), "request-1")
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "password") {
			t.Fatalf("backend failure = %d: %s", response.Code, response.Body)
		}
	})
}

func TestReadAndCancelRoutesReturnAttributableState(t *testing.T) {
	handler, repository, _, authorizer, _, _, _ := newExecutionHandler()
	create := sendJSON(handler, http.MethodPost, "/execution-pairs", marshalJSON(t, validCreateRequest()), "request-1")
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", create.Code, create.Body)
	}

	pair := httptest.NewRecorder()
	handler.ServeHTTP(pair, httptest.NewRequest(http.MethodGet, "/execution-pairs/pair-1", nil))
	if pair.Code != http.StatusOK || !strings.Contains(pair.Body.String(), `"contract":"atg.execution_pair_view"`) || !strings.Contains(pair.Body.String(), `"candidate_task"`) {
		t.Fatalf("get pair = %d: %s", pair.Code, pair.Body)
	}
	if authorizer.permission != PermissionReadPair || authorizer.resource != "pair-1" {
		t.Fatalf("pair authorization = (%q, %q)", authorizer.permission, authorizer.resource)
	}

	repository.tasks["task-base"] = withActiveAttempt(repository.tasks["task-base"])
	task := httptest.NewRecorder()
	handler.ServeHTTP(task, httptest.NewRequest(http.MethodGet, "/executions/task-base", nil))
	if task.Code != http.StatusOK || strings.Contains(task.Body.String(), "specification_bytes") || strings.Contains(task.Body.String(), "expected_test_ids") {
		t.Fatalf("get task = %d: %s", task.Code, task.Body)
	}

	repository.tasks["task-candidate"] = withActiveAttempt(repository.tasks["task-candidate"])
	cancelBody := marshalJSON(t, CancelRequest{Contract: "atg.execution_cancel_request", Version: "1.0", ExpectedBaseTaskRevision: 1, ExpectedCandidateTaskRevision: 1})
	cancel := sendJSON(handler, http.MethodPost, "/execution-pairs/pair-1/cancel", cancelBody, "")
	if cancel.Code != http.StatusAccepted || strings.Count(cancel.Body.String(), `"status":"cancel_requested"`) != 2 {
		t.Fatalf("cancel = %d: %s", cancel.Code, cancel.Body)
	}
	retry := sendJSON(handler, http.MethodPost, "/execution-pairs/pair-1/cancel", cancelBody, "")
	if retry.Code != http.StatusAccepted || strings.Count(retry.Body.String(), `"revision":2`) != 2 {
		t.Fatalf("idempotent cancel retry = %d: %s", retry.Code, retry.Body)
	}
	if authorizer.permission != PermissionCancelPair || authorizer.resource != "pair-1" {
		t.Fatalf("cancel authorization = (%q, %q)", authorizer.permission, authorizer.resource)
	}
}

func TestCancelPairRevisionConflictDoesNotPartiallyCancel(t *testing.T) {
	handler, repository, _, _, _, _, _ := newExecutionHandler()
	create := sendJSON(handler, http.MethodPost, "/execution-pairs", marshalJSON(t, validCreateRequest()), "request-1")
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", create.Code, create.Body)
	}
	repository.tasks["task-base"] = withActiveAttempt(repository.tasks["task-base"])
	repository.tasks["task-candidate"] = withActiveAttempt(repository.tasks["task-candidate"])
	body := marshalJSON(t, CancelRequest{Contract: "atg.execution_cancel_request", Version: "1.0", ExpectedBaseTaskRevision: 1, ExpectedCandidateTaskRevision: 2})
	response := sendJSON(handler, http.MethodPost, "/execution-pairs/pair-1/cancel", body, "")
	if response.Code != http.StatusConflict || repository.tasks["task-base"].Status != "running" || repository.tasks["task-candidate"].Status != "running" {
		t.Fatalf("stale cancellation status/tasks = %d/%q/%q: %s", response.Code, repository.tasks["task-base"].Status, repository.tasks["task-candidate"].Status, response.Body)
	}
}

func TestExecutionRoutesFailClosedWhenUnconfigured(t *testing.T) {
	response := httptest.NewRecorder()
	NewHandler(Dependencies{}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/execution-pairs", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured create status = %d, body %s", response.Code, response.Body)
	}
}

func newExecutionHandler() (http.Handler, *fakeExecutionRepository, *fakeAuthenticator, *fakeAuthorizer, *fakePairResolver, *fakeAuditRecorder, time.Time) {
	now := time.Date(2026, time.November, 17, 20, 0, 0, 0, time.UTC)
	repository := &fakeExecutionRepository{
		pairs: make(map[string]execution.PairRecord), tasks: make(map[string]execution.TaskRecord),
		idempotency: make(map[string]execution.PairRecord), cancellationRevisions: make(map[string]int64),
	}
	authenticator := &fakeAuthenticator{principal: Principal{Identity: execution.Identity{Kind: "person", ID: "user-1"}, Role: "execution-operator", AuthorizationRef: "authz-1"}}
	authorizer := &fakeAuthorizer{}
	resolver := &fakePairResolver{}
	audit := &fakeAuditRecorder{}
	api := &ExecutionAPI{
		Repository: repository, Authenticator: authenticator, Authorizer: authorizer,
		Resolver: resolver, Audit: audit, Clock: fixedAPITime{now},
	}
	return NewHandler(Dependencies{Execution: api}), repository, authenticator, authorizer, resolver, audit, now
}

func readyExecutionAPI() *ExecutionAPI {
	_, repository, authenticator, authorizer, resolver, audit, now := newExecutionHandler()
	return &ExecutionAPI{
		Repository: repository, Authenticator: authenticator, Authorizer: authorizer,
		Resolver: resolver, Audit: audit, Clock: fixedAPITime{now},
	}
}

func validCreateRequest() CreatePairRequest {
	return CreatePairRequest{
		Contract: "atg.execution_pair_request", Version: "1.0", RunID: "run-1", ExpectedRunRevision: 3,
		GenerationID: "generation-1", ProfileID: "spring-profile", ProfileVersion: "1.2.3",
		SelectionID: "focused-selection", ComparisonMode: "same_subject_control",
	}
}

func pairInputForRequest(request CreatePairRequest) (execution.PairInput, error) {
	base := sampleTaskSpec(request, "task-base", "base")
	candidate := sampleTaskSpec(request, "task-candidate", "candidate")
	baseBytes, err := json.Marshal(base)
	if err != nil {
		return execution.PairInput{}, err
	}
	candidateBytes, err := json.Marshal(candidate)
	if err != nil {
		return execution.PairInput{}, err
	}
	return execution.PairInput{BaseSpec: baseBytes, CandidateSpec: candidateBytes}, nil
}

func sampleTaskSpec(request CreatePairRequest, taskID, role string) execution.TaskSpec {
	manifestDigest := testDigest("1")
	profileDigest := testDigest("2")
	return execution.TaskSpec{
		Contract: "atg.execution_task_spec", Version: "1.0", TaskSpecID: taskID,
		RunID: request.RunID, ExpectedRunRevision: request.ExpectedRunRevision, PairID: "pair-1",
		GenerationID: request.GenerationID, SubjectRole: role, ComparisonMode: request.ComparisonMode,
		Subject: execution.Subject{
			Source:   execution.Source{Identity: execution.Identity{Kind: "git-commit", ID: "commit-1", Digest: testDigest("3")}, Locator: "https://example.invalid/source.git"},
			Identity: execution.Identity{Kind: "source-tree", ID: "tree-1", Digest: testDigest("4")},
		},
		Generation: execution.GenerationRefs{
			Manifest: execution.Artifact{ArtifactID: "manifest-1", MediaType: "application/vnd.atg.generation+json", Digest: manifestDigest},
			Patch:    execution.Artifact{ArtifactID: "patch-1", MediaType: "text/x-diff", Digest: testDigest("5")},
			Tests:    execution.Artifact{ArtifactID: "tests-1", MediaType: "application/vnd.atg.materialized-tests+json", Digest: testDigest("6")},
		},
		CodeApproval: execution.CodeApproval{
			CheckpointID: "checkpoint-1", DecisionID: "code-decision-1",
			Reviewer:                 execution.Reviewer{Identity: execution.Identity{Kind: "person", ID: "reviewer-1"}, Role: "test-reviewer", AuthorizationRef: "code-auth-1"},
			ReviewedGenerationDigest: manifestDigest,
		},
		Profile: execution.ProfileRef{ProfileID: request.ProfileID, ProfileVersion: request.ProfileVersion,
			Artifact: execution.Artifact{ArtifactID: "profile-1", MediaType: "application/vnd.atg.execution-profile+json", Digest: profileDigest}},
		ProfileApproval: execution.ProfileApproval{
			DecisionID: "profile-decision-1", Reviewer: execution.Reviewer{Identity: execution.Identity{Kind: "person", ID: "reviewer-2"}, Role: "profile-reviewer", AuthorizationRef: "profile-auth-1"},
			ApprovedProfileDigest: profileDigest,
		},
		ExpectedTestIDs: []string{"sample.SmokeTest#works"},
		RetryPolicy:     execution.RetryPolicy{PolicyID: "retry-1", MaxAttempts: 1, DeadlineAt: "2026-11-18T20:00:00Z", MaxBackoffSeconds: 30, RetryableFailures: []string{}},
		CreatedAt:       "2026-11-17T20:00:00Z",
	}
}

func withActiveAttempt(task execution.TaskRecord) execution.TaskRecord {
	task.Status = "running"
	task.ActiveAttemptID = "attempt-1"
	task.Revision = 1
	return task
}

func testDigest(char string) string { return "sha256:" + strings.Repeat(char, 64) }

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sendJSON(handler http.Handler, method, path string, body []byte, idempotencyKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

type fixedAPITime struct{ now time.Time }

func (c fixedAPITime) Now() time.Time { return c.now }

type fakeAuthenticator struct {
	principal Principal
	err       error
	calls     int
}

func (a *fakeAuthenticator) Authenticate(context.Context, *http.Request) (Principal, error) {
	a.calls++
	return a.principal, a.err
}

type fakeAuthorizer struct {
	permission Permission
	resource   string
	err        error
}

func (a *fakeAuthorizer) Authorize(_ context.Context, _ Principal, permission Permission, resource string) error {
	a.permission, a.resource = permission, resource
	return a.err
}

type fakePairResolver struct {
	calls  int
	mutate func(*execution.PairInput)
	err    error
}

func (r *fakePairResolver) ResolvePair(_ context.Context, _ Principal, request CreatePairRequest) (execution.PairInput, error) {
	r.calls++
	input, err := pairInputForRequest(request)
	if err != nil {
		return execution.PairInput{}, err
	}
	if r.mutate != nil {
		r.mutate(&input)
	}
	return input, r.err
}

type fakeAuditRecorder struct {
	calls int
	last  AuditEvent
	err   error
}

func (a *fakeAuditRecorder) RecordAuthorization(_ context.Context, event AuditEvent) error {
	a.calls++
	a.last = event
	return a.err
}

type fakeExecutionRepository struct {
	pairs                 map[string]execution.PairRecord
	tasks                 map[string]execution.TaskRecord
	idempotency           map[string]execution.PairRecord
	lastInput             execution.PairInput
	createCalls           int
	createErr             error
	cancellationRevisions map[string]int64
}

func (r *fakeExecutionRepository) CreatePair(_ context.Context, input execution.PairInput) (execution.PairRecord, bool, error) {
	r.createCalls++
	r.lastInput = input
	if r.createErr != nil {
		return execution.PairRecord{}, false, r.createErr
	}
	base, err := execution.DecodeTaskSpec(input.BaseSpec)
	if err != nil {
		return execution.PairRecord{}, false, err
	}
	candidate, err := execution.DecodeTaskSpec(input.CandidateSpec)
	if err != nil {
		return execution.PairRecord{}, false, err
	}
	key := base.RunID + "\x00" + input.IdempotencyKey
	digest := input.RequestDigest()
	if pair, found := r.idempotency[key]; found {
		if pair.RequestDigest != digest {
			return execution.PairRecord{}, false, execution.ErrIdempotencyConflict
		}
		return pair, false, nil
	}
	pair := execution.PairRecord{
		PairID: base.PairID, RunID: base.RunID, GenerationID: base.GenerationID,
		BaseTaskID: base.TaskSpecID, CandidateTaskID: candidate.TaskSpecID,
		RequestDigest: digest, CreatedAt: input.CreatedAt,
	}
	r.pairs[pair.PairID] = pair
	r.idempotency[key] = pair
	r.tasks[base.TaskSpecID] = newTaskRecord(base, input.BaseSpec, input.CreatedAt)
	r.tasks[candidate.TaskSpecID] = newTaskRecord(candidate, input.CandidateSpec, input.CreatedAt)
	return pair, true, nil
}

func (r *fakeExecutionRepository) GetPair(_ context.Context, pairID string) (execution.PairRecord, error) {
	pair, ok := r.pairs[pairID]
	if !ok {
		return execution.PairRecord{}, execution.ErrPairNotFound
	}
	return pair, nil
}

func (r *fakeExecutionRepository) GetTask(_ context.Context, taskID string) (execution.TaskRecord, error) {
	task, ok := r.tasks[taskID]
	if !ok {
		return execution.TaskRecord{}, execution.ErrTaskNotFound
	}
	return task, nil
}

func (r *fakeExecutionRepository) RequestCancelPair(_ context.Context, pairID string, baseExpectedRevision, candidateExpectedRevision int64, now time.Time) (execution.PairRecord, execution.TaskRecord, execution.TaskRecord, error) {
	pair, found := r.pairs[pairID]
	if !found {
		return execution.PairRecord{}, execution.TaskRecord{}, execution.TaskRecord{}, execution.ErrPairNotFound
	}
	base := r.tasks[pair.BaseTaskID]
	candidate := r.tasks[pair.CandidateTaskID]
	baseReplay, err := r.cancellationReplay(base, baseExpectedRevision)
	if err != nil {
		return execution.PairRecord{}, execution.TaskRecord{}, execution.TaskRecord{}, err
	}
	candidateReplay, err := r.cancellationReplay(candidate, candidateExpectedRevision)
	if err != nil {
		return execution.PairRecord{}, execution.TaskRecord{}, execution.TaskRecord{}, err
	}
	if !baseReplay {
		base = r.cancelTask(base, baseExpectedRevision, now)
	}
	if !candidateReplay {
		candidate = r.cancelTask(candidate, candidateExpectedRevision, now)
	}
	r.tasks[base.TaskID] = base
	r.tasks[candidate.TaskID] = candidate
	return pair, base, candidate, nil
}

func (r *fakeExecutionRepository) cancellationReplay(task execution.TaskRecord, expectedRevision int64) (bool, error) {
	if task.Revision == expectedRevision {
		return false, nil
	}
	if priorRevision, found := r.cancellationRevisions[task.TaskID]; found && priorRevision == expectedRevision {
		return true, nil
	}
	return false, execution.ErrStaleRevision
}

func (r *fakeExecutionRepository) cancelTask(task execution.TaskRecord, expectedRevision int64, now time.Time) execution.TaskRecord {
	if task.Status == "completed" || task.Status == "failed" || task.Status == "blocked" || task.Status == "not_reached" || task.Status == "cancelled" || task.Status == "superseded" || task.Status == "cancel_requested" {
		return task
	}
	task.Status = "cancel_requested"
	task.Revision++
	task.UpdatedAt = now
	r.cancellationRevisions[task.TaskID] = expectedRevision
	return task
}

func newTaskRecord(spec execution.TaskSpec, bytes []byte, now time.Time) execution.TaskRecord {
	return execution.TaskRecord{
		TaskID: spec.TaskSpecID, RunID: spec.RunID, PairID: spec.PairID, SubjectRole: spec.SubjectRole,
		Specification: append([]byte(nil), bytes...), SpecificationDigest: execution.DigestTaskSpec(bytes),
		Status: "queued", Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
