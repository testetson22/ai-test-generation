package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
)

const maxExecutionRequestBytes = 16 * 1024

var (
	ErrUnauthenticated   = errors.New("request is unauthenticated")
	ErrForbidden         = errors.New("request is forbidden")
	ErrReferenceNotFound = errors.New("execution reference not found")
	ErrReferenceInvalid  = errors.New("execution reference is invalid")
	apiIDPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	apiVersionPattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

type Permission string

const (
	PermissionCreatePair Permission = "execution_pair:create"
	PermissionReadPair   Permission = "execution_pair:read"
	PermissionReadTask   Permission = "execution:read"
	PermissionCancelPair Permission = "execution_pair:cancel"
)

type Principal struct {
	Identity         execution.Identity `json:"identity"`
	Role             string             `json:"role"`
	AuthorizationRef string             `json:"authorization_ref"`
}

type Authenticator interface {
	Authenticate(context.Context, *http.Request) (Principal, error)
}

type Authorizer interface {
	Authorize(context.Context, Principal, Permission, string) error
}

// PairResolver resolves authorized run/generation/profile/selection references
// into canonical task specifications. It must verify selection_id and must not
// trust a caller-supplied task specification or profile document.
type PairResolver interface {
	ResolvePair(context.Context, Principal, CreatePairRequest) (execution.PairInput, error)
}

type AuditRecorder interface {
	RecordAuthorization(context.Context, AuditEvent) error
}

type AuditEvent struct {
	Principal      Principal
	Permission     Permission
	Resource       string
	IdempotencyKey string
	RequestDigest  string
	AuthorizedAt   time.Time
}

type ExecutionRepository interface {
	CreatePair(context.Context, execution.PairInput) (execution.PairRecord, bool, error)
	GetPair(context.Context, string) (execution.PairRecord, error)
	GetTask(context.Context, string) (execution.TaskRecord, error)
	RequestCancelPair(context.Context, string, int64, int64, time.Time) (execution.PairRecord, execution.TaskRecord, execution.TaskRecord, error)
}

type ExecutionAPI struct {
	Repository    ExecutionRepository
	Authenticator Authenticator
	Authorizer    Authorizer
	Resolver      PairResolver
	Audit         AuditRecorder
	Clock         execution.Clock
}

type CreatePairRequest struct {
	Contract            string `json:"contract"`
	Version             string `json:"version"`
	RunID               string `json:"run_id"`
	ExpectedRunRevision int64  `json:"expected_run_revision"`
	GenerationID        string `json:"generation_id"`
	ProfileID           string `json:"profile_id"`
	ProfileVersion      string `json:"profile_version"`
	SelectionID         string `json:"selection_id"`
	ComparisonMode      string `json:"comparison_mode"`
}

type CancelRequest struct {
	Contract                      string `json:"contract"`
	Version                       string `json:"version"`
	ExpectedBaseTaskRevision      int64  `json:"expected_base_task_revision"`
	ExpectedCandidateTaskRevision int64  `json:"expected_candidate_task_revision"`
}

type PairResponse struct {
	Contract      string       `json:"contract"`
	Version       string       `json:"version"`
	PairID        string       `json:"pair_id"`
	RunID         string       `json:"run_id"`
	GenerationID  string       `json:"generation_id"`
	RequestDigest string       `json:"request_digest"`
	BaseTask      TaskResponse `json:"base_task"`
	CandidateTask TaskResponse `json:"candidate_task"`
	CreatedAt     string       `json:"created_at"`
}

type TaskResponse struct {
	Contract        string              `json:"contract"`
	Version         string              `json:"version"`
	TaskID          string              `json:"task_id"`
	RunID           string              `json:"run_id"`
	PairID          string              `json:"pair_id"`
	SubjectRole     string              `json:"subject_role"`
	Specification   execution.Artifact  `json:"specification"`
	Status          string              `json:"status"`
	Revision        int64               `json:"revision"`
	AttemptCount    int                 `json:"attempt_count"`
	ActiveAttemptID string              `json:"active_attempt_id,omitempty"`
	ExecutionResult *execution.Artifact `json:"execution_result,omitempty"`
	TerminalReason  string              `json:"terminal_reason,omitempty"`
	CreatedAt       string              `json:"created_at"`
	UpdatedAt       string              `json:"updated_at"`
}

type apiError struct {
	Contract string `json:"contract"`
	Version  string `json:"version"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

func (a *ExecutionAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /execution-pairs", a.createPair)
	mux.HandleFunc("GET /execution-pairs/{pairID}", a.getPair)
	mux.HandleFunc("POST /execution-pairs/{pairID}/cancel", a.cancelPair)
	mux.HandleFunc("GET /executions/{taskID}", a.getTask)
}

func (a *ExecutionAPI) Ready() bool {
	return a != nil && a.Repository != nil && a.Authenticator != nil && a.Authorizer != nil &&
		a.Resolver != nil && a.Audit != nil
}

func (a *ExecutionAPI) createPair(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.Repository == nil || a.Authenticator == nil || a.Authorizer == nil || a.Resolver == nil || a.Audit == nil {
		unavailable(w, r)
		return
	}
	body, err := decodeJSONBody(w, r, maxExecutionRequestBytes)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var request CreatePairRequest
	if err := decodeOneJSON(body, &request); err != nil || !request.valid() {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "request references or contract version are invalid")
		return
	}
	idempotencyKey, err := requiredIdempotencyKey(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "a valid Idempotency-Key header is required")
		return
	}
	principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.authorize(w, r, principal, PermissionCreatePair, request.RunID) {
		return
	}
	now := a.now()
	requestDigest := digestBytes(body)
	if err := a.Audit.RecordAuthorization(r.Context(), AuditEvent{
		Principal: principal, Permission: PermissionCreatePair, Resource: request.RunID,
		IdempotencyKey: idempotencyKey, RequestDigest: requestDigest, AuthorizedAt: now,
	}); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "audit_unavailable", "authorization audit is unavailable")
		return
	}
	input, err := a.Resolver.ResolvePair(r.Context(), principal, request)
	if err != nil {
		a.writeOperationError(w, err)
		return
	}
	input.IdempotencyKey = idempotencyKey
	input.CreatedAt = now
	if err := validateResolvedPair(request, input); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "resolved_pair_invalid", "approved references did not resolve to a matching execution pair")
		return
	}
	pair, created, err := a.Repository.CreatePair(r.Context(), input)
	if err != nil {
		a.writeOperationError(w, err)
		return
	}
	response, err := a.pairResponse(r.Context(), pair)
	if err != nil {
		a.writeOperationError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", "/execution-pairs/"+url.PathEscape(pair.PairID))
	}
	writeJSON(w, status, response)
}

func (a *ExecutionAPI) getPair(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.Repository == nil || a.Authenticator == nil || a.Authorizer == nil {
		unavailable(w, r)
		return
	}
	pairID := r.PathValue("pairID")
	principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.authorize(w, r, principal, PermissionReadPair, pairID) {
		return
	}
	pair, err := a.Repository.GetPair(r.Context(), pairID)
	if err != nil {
		a.writeOperationError(w, err)
		return
	}
	response, err := a.pairResponse(r.Context(), pair)
	if err != nil {
		a.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *ExecutionAPI) getTask(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.Repository == nil || a.Authenticator == nil || a.Authorizer == nil {
		unavailable(w, r)
		return
	}
	taskID := r.PathValue("taskID")
	principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.authorize(w, r, principal, PermissionReadTask, taskID) {
		return
	}
	task, err := a.Repository.GetTask(r.Context(), taskID)
	if err != nil {
		a.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskResponse(task))
}

func (a *ExecutionAPI) cancelPair(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.Repository == nil || a.Authenticator == nil || a.Authorizer == nil || a.Audit == nil {
		unavailable(w, r)
		return
	}
	body, err := decodeJSONBody(w, r, maxExecutionRequestBytes)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var request CancelRequest
	if err := decodeOneJSON(body, &request); err != nil || request.Contract != "atg.execution_cancel_request" || request.Version != "1.0" || request.ExpectedBaseTaskRevision < 1 || request.ExpectedCandidateTaskRevision < 1 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "cancellation contract or expected revision is invalid")
		return
	}
	pairID := r.PathValue("pairID")
	principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.authorize(w, r, principal, PermissionCancelPair, pairID) {
		return
	}
	now := a.now()
	if err := a.Audit.RecordAuthorization(r.Context(), AuditEvent{
		Principal: principal, Permission: PermissionCancelPair, Resource: pairID,
		RequestDigest: digestBytes(body), AuthorizedAt: now,
	}); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "audit_unavailable", "authorization audit is unavailable")
		return
	}
	pair, base, candidate, err := a.Repository.RequestCancelPair(r.Context(), pairID, request.ExpectedBaseTaskRevision, request.ExpectedCandidateTaskRevision, now)
	if err != nil {
		a.writeOperationError(w, err)
		return
	}
	status := http.StatusAccepted
	if terminalTaskStatus(base.Status) && terminalTaskStatus(candidate.Status) {
		status = http.StatusOK
	}
	if !pairTasksMatch(pair, base, candidate) {
		a.writeOperationError(w, fmt.Errorf("persisted pair and task identities do not match"))
		return
	}
	writeJSON(w, status, pairResponse(pair, base, candidate))
}

func (a *ExecutionAPI) pairResponse(ctx context.Context, pair execution.PairRecord) (PairResponse, error) {
	base, err := a.Repository.GetTask(ctx, pair.BaseTaskID)
	if err != nil {
		return PairResponse{}, err
	}
	candidate, err := a.Repository.GetTask(ctx, pair.CandidateTaskID)
	if err != nil {
		return PairResponse{}, err
	}
	if !pairTasksMatch(pair, base, candidate) {
		return PairResponse{}, fmt.Errorf("persisted pair and task identities do not match")
	}
	return pairResponse(pair, base, candidate), nil
}

func pairResponse(pair execution.PairRecord, base, candidate execution.TaskRecord) PairResponse {
	return PairResponse{
		Contract: "atg.execution_pair_view", Version: "1.0",
		PairID: pair.PairID, RunID: pair.RunID, GenerationID: pair.GenerationID, RequestDigest: pair.RequestDigest,
		BaseTask: taskResponse(base), CandidateTask: taskResponse(candidate),
		CreatedAt: pair.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func pairTasksMatch(pair execution.PairRecord, base, candidate execution.TaskRecord) bool {
	return base.TaskID == pair.BaseTaskID && base.PairID == pair.PairID && base.RunID == pair.RunID && base.SubjectRole == "base" &&
		candidate.TaskID == pair.CandidateTaskID && candidate.PairID == pair.PairID && candidate.RunID == pair.RunID && candidate.SubjectRole == "candidate"
}

func terminalTaskStatus(status string) bool {
	switch status {
	case "completed", "failed", "blocked", "not_reached", "cancelled", "superseded":
		return true
	default:
		return false
	}
}

func taskResponse(task execution.TaskRecord) TaskResponse {
	return TaskResponse{
		Contract: "atg.execution_task_view", Version: "1.0",
		TaskID: task.TaskID, RunID: task.RunID, PairID: task.PairID, SubjectRole: task.SubjectRole,
		Specification: execution.Artifact{
			ArtifactID: task.TaskID, MediaType: "application/vnd.atg.execution-task-spec+json",
			Digest: task.SpecificationDigest, SizeBytes: int64(len(task.Specification)),
		},
		Status: task.Status, Revision: task.Revision, AttemptCount: task.AttemptCount,
		ActiveAttemptID: task.ActiveAttemptID, ExecutionResult: task.ExecutionResult,
		TerminalReason: task.TerminalReason, CreatedAt: task.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: task.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (a *ExecutionAPI) authenticate(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	principal, err := a.Authenticator.Authenticate(r.Context(), r)
	if errors.Is(err, ErrUnauthenticated) {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "authentication is required")
		return Principal{}, false
	}
	if err != nil || !validPrincipal(principal) {
		writeAPIError(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is unavailable")
		return Principal{}, false
	}
	return principal, true
}

func (a *ExecutionAPI) authorize(w http.ResponseWriter, r *http.Request, principal Principal, permission Permission, resource string) bool {
	err := a.Authorizer.Authorize(r.Context(), principal, permission, resource)
	if errors.Is(err, ErrForbidden) {
		writeAPIError(w, http.StatusForbidden, "forbidden", "operation is not authorized")
		return false
	}
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "authorization_unavailable", "authorization is unavailable")
		return false
	}
	return true
}

func (a *ExecutionAPI) writeOperationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "authentication is required")
	case errors.Is(err, ErrForbidden):
		writeAPIError(w, http.StatusForbidden, "forbidden", "operation is not authorized")
	case errors.Is(err, execution.ErrPairNotFound), errors.Is(err, execution.ErrTaskNotFound), errors.Is(err, ErrReferenceNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "execution resource was not found")
	case errors.Is(err, execution.ErrIdempotencyConflict), errors.Is(err, execution.ErrStaleRevision):
		writeAPIError(w, http.StatusConflict, "conflict", "request conflicts with current execution state")
	case errors.Is(err, execution.ErrInvalidTaskSpec), errors.Is(err, ErrReferenceInvalid):
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_reference", "approved execution references are invalid")
	default:
		writeAPIError(w, http.StatusServiceUnavailable, "execution_unavailable", "execution service is unavailable")
	}
}

func (a *ExecutionAPI) now() time.Time {
	if a.Clock == nil {
		return time.Now().UTC()
	}
	return a.Clock.Now().UTC()
}

func (request CreatePairRequest) valid() bool {
	return request.Contract == "atg.execution_pair_request" && request.Version == "1.0" &&
		apiIDPattern.MatchString(request.RunID) && request.ExpectedRunRevision > 0 &&
		apiIDPattern.MatchString(request.GenerationID) && apiIDPattern.MatchString(request.ProfileID) &&
		len(request.ProfileVersion) <= 64 && apiVersionPattern.MatchString(request.ProfileVersion) && apiIDPattern.MatchString(request.SelectionID) &&
		(request.ComparisonMode == "same_subject_control" || request.ComparisonMode == "differential")
}

func validateResolvedPair(request CreatePairRequest, input execution.PairInput) error {
	base, err := execution.DecodeTaskSpec(input.BaseSpec)
	if err != nil {
		return err
	}
	candidate, err := execution.DecodeTaskSpec(input.CandidateSpec)
	if err != nil {
		return err
	}
	if err := execution.ValidatePair(base, candidate); err != nil {
		return err
	}
	for _, spec := range []execution.TaskSpec{base, candidate} {
		if spec.RunID != request.RunID || spec.ExpectedRunRevision != request.ExpectedRunRevision ||
			spec.GenerationID != request.GenerationID || spec.Profile.ProfileID != request.ProfileID ||
			spec.Profile.ProfileVersion != request.ProfileVersion || spec.ComparisonMode != request.ComparisonMode {
			return ErrReferenceInvalid
		}
	}
	return nil
}

func validPrincipal(principal Principal) bool {
	return principal.Identity.Kind != "" && len(principal.Identity.Kind) <= 128 && !strings.ContainsAny(principal.Identity.Kind, "\x00\t\n\v\f\r") &&
		apiIDPattern.MatchString(principal.Identity.ID) && principal.Role != "" && len(principal.Role) <= 128 &&
		strings.TrimSpace(principal.Role) == principal.Role && !strings.ContainsAny(principal.Role, "\x00\t\n\v\f\r") &&
		apiIDPattern.MatchString(principal.AuthorizationRef)
}

func requiredIdempotencyKey(r *http.Request) (string, error) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		return "", errors.New("exactly one idempotency key is required")
	}
	key := strings.TrimSpace(values[0])
	if key != values[0] || !apiIDPattern.MatchString(key) {
		return "", errors.New("idempotency key is invalid")
	}
	return key, nil
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, errors.New("content type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("request body is empty")
	}
	return body, nil
}

func writeBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the size limit")
		return
	}
	writeAPIError(w, http.StatusBadRequest, "invalid_request", "request body is invalid")
}

func decodeOneJSON(body []byte, output any) error {
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}

func rejectDuplicateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if strings.ToLower(key) != key {
				return fmt.Errorf("non-canonical JSON object key %q", key)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("malformed JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("malformed JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func digestBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	response := apiError{Contract: "atg.execution_error", Version: "1.0", Code: code, Message: message}
	writeJSON(w, status, response)
}
