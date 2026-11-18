package testgenflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const DefaultMaxGenerationAttempts = 6

var (
	ErrInvalidRequest      = errors.New("invalid test-generation development request")
	ErrInvalidRun          = errors.New("invalid test-generation development run")
	ErrInvalidEvent        = errors.New("invalid test-generation development event")
	ErrInvalidTransition   = errors.New("invalid test-generation development transition")
	ErrRevisionConflict    = errors.New("test-generation development run revision conflict")
	ErrRunNotFound         = errors.New("test-generation development run not found")
	ErrIdempotencyConflict = errors.New("test-generation development idempotency conflict")
	ErrEventConflict       = errors.New("test-generation development event conflict")
	ErrRunCorrupt          = errors.New("stored test-generation development run is corrupt")

	flowIDPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	commitPattern         = regexp.MustCompile(`^(?:[A-Fa-f0-9]{40}|[A-Fa-f0-9]{64})$`)
	sha256Pattern         = regexp.MustCompile(`^sha256:[A-Fa-f0-9]{64}$`)
	profileVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	issueIIDPattern       = regexp.MustCompile(`^[0-9]{1,20}$`)
	branchNameRegexp      = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)
)

type Status string

const (
	StatusQueued          Status = "queued"
	StatusRunning         Status = "running"
	StatusCancelRequested Status = "cancel_requested"
	StatusCompleted       Status = "completed"
	StatusFailed          Status = "failed"
	StatusCancelled       Status = "cancelled"
	StatusOutcomeUnknown  Status = "outcome_unknown"
)

type Stage string

const (
	StageSource       Stage = "source"
	StageVerifyCommit Stage = "verify_commit"
	StageBaseline     Stage = "baseline_package"
	StageGenerate     Stage = "generate_tests"
	StageFocusedTest  Stage = "focused_test"
	StageFinalVerify  Stage = "final_verify"
	StageOpenPR       Stage = "open_pr"
	StageDone         Stage = "done"
)

type AttemptStatus string

const (
	AttemptRunning   AttemptStatus = "running"
	AttemptGenerated AttemptStatus = "generated"
	AttemptAccepted  AttemptStatus = "accepted"
	AttemptNoTests   AttemptStatus = "no_tests"
	AttemptFailed    AttemptStatus = "failed"
)

type EventType string

const (
	EventSourceResolved       EventType = "source_resolved"
	EventSignatureVerified    EventType = "signature_verified"
	EventBaselineCompleted    EventType = "baseline_completed"
	EventGenerationStarted    EventType = "generation_started"
	EventGenerationCompleted  EventType = "generation_completed"
	EventFocusedTestCompleted EventType = "focused_test_completed"
	EventFinalVerifyCompleted EventType = "final_verify_completed"
	EventPRCompleted          EventType = "pr_completed"
	EventStageFailed          EventType = "stage_failed"
	EventCancelRequested      EventType = "cancel_requested"
	EventCancelled            EventType = "cancelled"
	EventOutcomeUnknown       EventType = "outcome_unknown"
)

type Request struct {
	RepositoryURL  string `json:"repository_url"`
	Revision       string `json:"revision"`
	BaseBranch     string `json:"base_branch"`
	IssueIID       string `json:"issue_iid,omitempty"`
	Subdirectory   string `json:"subdirectory"`
	ProfileID      string `json:"profile_id"`
	ProfileVersion string `json:"profile_version"`
	ProfileDigest  string `json:"profile_digest"`
	VerifyCommit   bool   `json:"verify_commit"`
}

type GenerationAttempt struct {
	Number                 int           `json:"number"`
	Status                 AttemptStatus `json:"status"`
	ChangedTestPaths       []string      `json:"changed_test_paths,omitempty"`
	PatchDigest            string        `json:"patch_digest,omitempty"`
	FocusedTestEvidenceRef string        `json:"focused_test_evidence_ref,omitempty"`
	LastError              string        `json:"last_error,omitempty"`
	StartedAt              time.Time     `json:"started_at"`
	FinishedAt             *time.Time    `json:"finished_at,omitempty"`
}

type Run struct {
	RunID             string              `json:"run_id"`
	IdempotencyKey    string              `json:"idempotency_key"`
	RequestDigest     string              `json:"request_digest"`
	Request           Request             `json:"request"`
	Status            Status              `json:"status"`
	Stage             Stage               `json:"stage"`
	Revision          int64               `json:"revision"`
	MaxAttempts       int                 `json:"max_attempts"`
	ResolvedCommit    string              `json:"resolved_commit,omitempty"`
	Attempts          []GenerationAttempt `json:"generation_attempts"`
	TestsAdded        int                 `json:"tests_added"`
	ChangedTestPaths  []string            `json:"changed_test_paths"`
	PatchDigest       string              `json:"patch_digest,omitempty"`
	PRURL             string              `json:"pr_url"`
	EvidenceRef       string              `json:"evidence_ref,omitempty"`
	LastError         string              `json:"last_error,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
	CancelRequestedAt *time.Time          `json:"cancel_requested_at,omitempty"`
}

type Event struct {
	ID                   string    `json:"event_id"`
	ExpectedRevision     int64     `json:"expected_revision"`
	Type                 EventType `json:"event_type"`
	OccurredAt           time.Time `json:"occurred_at"`
	ResolvedCommit       string    `json:"resolved_commit,omitempty"`
	Succeeded            bool      `json:"succeeded,omitempty"`
	ChangedTestPaths     []string  `json:"changed_test_paths,omitempty"`
	PatchDigest          string    `json:"patch_digest,omitempty"`
	Diagnostic           string    `json:"diagnostic,omitempty"`
	PRURL                string    `json:"pr_url,omitempty"`
	EvidenceRef          string    `json:"evidence_ref,omitempty"`
	TerminationConfirmed bool      `json:"termination_confirmed,omitempty"`
}

func NewRun(runID, idempotencyKey string, request Request, maxAttempts int, now time.Time) (Run, error) {
	request.Revision = strings.ToLower(request.Revision)
	if err := request.Validate(); err != nil {
		return Run{}, err
	}
	if !validFlowIdent(runID) || !validFlowIdent(idempotencyKey) || strings.TrimSpace(idempotencyKey) != idempotencyKey {
		return Run{}, fmt.Errorf("%w: run and idempotency identities are required", ErrInvalidRequest)
	}
	if maxAttempts < 1 || maxAttempts > DefaultMaxGenerationAttempts {
		return Run{}, fmt.Errorf("%w: generation attempts must be between 1 and %d", ErrInvalidRequest, DefaultMaxGenerationAttempts)
	}
	if now.IsZero() {
		return Run{}, fmt.Errorf("%w: creation time is required", ErrInvalidRequest)
	}
	now = now.UTC().Truncate(time.Microsecond)
	requestDigest, err := requestDigest(request, maxAttempts)
	if err != nil {
		return Run{}, fmt.Errorf("%w: encode request: %v", ErrInvalidRequest, err)
	}
	run := Run{
		RunID:            runID,
		IdempotencyKey:   idempotencyKey,
		RequestDigest:    requestDigest,
		Request:          request,
		Status:           StatusQueued,
		Stage:            StageSource,
		Revision:         1,
		MaxAttempts:      maxAttempts,
		Attempts:         []GenerationAttempt{},
		ChangedTestPaths: []string{},
		PRURL:            "",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := run.Validate(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (r Request) Validate() error {
	repository, err := url.Parse(r.RepositoryURL)
	if err != nil || repository.Scheme != "https" || repository.Host == "" || repository.User != nil || repository.RawQuery != "" || repository.Fragment != "" {
		return fmt.Errorf("%w: repository URL must be credential-free HTTPS", ErrInvalidRequest)
	}
	if !commitPattern.MatchString(r.Revision) {
		return fmt.Errorf("%w: revision must be an immutable Git commit", ErrInvalidRequest)
	}
	if !validBranch(r.BaseBranch) {
		return fmt.Errorf("%w: base branch is invalid", ErrInvalidRequest)
	}
	if r.IssueIID != "" && !issueIIDPattern.MatchString(r.IssueIID) {
		return fmt.Errorf("%w: issue IID is invalid", ErrInvalidRequest)
	}
	if !validSubdirectory(r.Subdirectory) {
		return fmt.Errorf("%w: project subdirectory must be a normalized relative path", ErrInvalidRequest)
	}
	if !validFlowIdent(r.ProfileID) || !profileVersionPattern.MatchString(r.ProfileVersion) || !sha256Pattern.MatchString(r.ProfileDigest) {
		return fmt.Errorf("%w: immutable execution-profile identity is required", ErrInvalidRequest)
	}
	return nil
}

func (r Run) Apply(event Event) (Run, error) {
	if err := r.Validate(); err != nil {
		return Run{}, err
	}
	if !validFlowIdent(event.ID) || event.ExpectedRevision < 1 || event.OccurredAt.IsZero() || event.OccurredAt.Before(r.CreatedAt) {
		return Run{}, fmt.Errorf("%w: event identity, revision, or timestamp is invalid", ErrInvalidEvent)
	}
	if event.ExpectedRevision != r.Revision {
		return Run{}, ErrRevisionConflict
	}
	if !isActive(r.Status) && !(r.Status == StatusCancelRequested && (event.Type == EventCancelled || event.Type == EventOutcomeUnknown)) {
		return Run{}, fmt.Errorf("%w: run is already %s", ErrInvalidTransition, r.Status)
	}
	if event.OccurredAt.Before(r.UpdatedAt) {
		return Run{}, fmt.Errorf("%w: event timestamp precedes the current run state", ErrInvalidEvent)
	}
	event.OccurredAt = event.OccurredAt.UTC().Truncate(time.Microsecond)
	next := cloneRun(r)
	next.Revision++
	next.UpdatedAt = event.OccurredAt

	switch event.Type {
	case EventSourceResolved:
		if r.Status != StatusQueued || r.Stage != StageSource || !commitPattern.MatchString(event.ResolvedCommit) || !strings.EqualFold(event.ResolvedCommit, r.Request.Revision) {
			return Run{}, fmt.Errorf("%w: resolved source must match the pinned request revision", ErrInvalidTransition)
		}
		next.Status = StatusRunning
		next.ResolvedCommit = strings.ToLower(event.ResolvedCommit)
		if r.Request.VerifyCommit {
			next.Stage = StageVerifyCommit
		} else {
			next.Stage = StageBaseline
		}
	case EventSignatureVerified:
		if r.Status != StatusRunning || r.Stage != StageVerifyCommit {
			return Run{}, fmt.Errorf("%w: signature result is not expected at this stage", ErrInvalidTransition)
		}
		if !event.Succeeded {
			return failedRun(r, event, "source signature verification failed")
		}
		next.Stage = StageBaseline
	case EventBaselineCompleted:
		if r.Status != StatusRunning || r.Stage != StageBaseline {
			return Run{}, fmt.Errorf("%w: baseline result is not expected at this stage", ErrInvalidTransition)
		}
		if !event.Succeeded {
			return failedRun(r, event, "baseline Maven package failed")
		}
		next.Stage = StageGenerate
	case EventGenerationStarted:
		if r.Status != StatusRunning || r.Stage != StageGenerate || len(r.Attempts) >= r.MaxAttempts {
			return Run{}, fmt.Errorf("%w: generation attempt is not authorized", ErrInvalidTransition)
		}
		next.Attempts = append(next.Attempts, GenerationAttempt{
			Number:    len(r.Attempts) + 1,
			Status:    AttemptRunning,
			StartedAt: event.OccurredAt.UTC(),
		})
	case EventGenerationCompleted:
		if r.Status != StatusRunning || r.Stage != StageGenerate || len(r.Attempts) == 0 || r.Attempts[len(r.Attempts)-1].Status != AttemptRunning {
			return Run{}, fmt.Errorf("%w: generation result has no active attempt", ErrInvalidTransition)
		}
		paths, err := validateTestPaths(event.ChangedTestPaths)
		if err != nil {
			return Run{}, err
		}
		attempt := &next.Attempts[len(next.Attempts)-1]
		attempt.ChangedTestPaths = paths
		finishedAt := event.OccurredAt.UTC()
		attempt.FinishedAt = &finishedAt
		if len(paths) == 0 {
			if event.PatchDigest != "" {
				return Run{}, fmt.Errorf("%w: no-test generation cannot reference a patch", ErrInvalidEvent)
			}
			attempt.Status = AttemptNoTests
			next.TestsAdded = 0
			next.ChangedTestPaths = []string{}
			next.PatchDigest = ""
			next.LastError = ""
			next.Stage = StageDone
			next.Status = StatusCompleted
			break
		}
		if !sha256Pattern.MatchString(event.PatchDigest) {
			return Run{}, fmt.Errorf("%w: generated test patch digest is required", ErrInvalidEvent)
		}
		attempt.Status = AttemptGenerated
		attempt.PatchDigest = strings.ToLower(event.PatchDigest)
		next.Stage = StageFocusedTest
	case EventFocusedTestCompleted:
		if r.Status != StatusRunning || r.Stage != StageFocusedTest || len(r.Attempts) == 0 || r.Attempts[len(r.Attempts)-1].Status != AttemptGenerated {
			return Run{}, fmt.Errorf("%w: focused test result has no generated candidate", ErrInvalidTransition)
		}
		attempt := &next.Attempts[len(next.Attempts)-1]
		finishedAt := event.OccurredAt.UTC()
		attempt.FinishedAt = &finishedAt
		evidenceRef, err := boundedValue(event.EvidenceRef, 2048, "focused test evidence reference")
		if err != nil {
			return Run{}, err
		}
		if event.Succeeded {
			if evidenceRef == "" {
				return Run{}, fmt.Errorf("%w: successful focused tests require a report reference", ErrInvalidEvent)
			}
			attempt.Status = AttemptAccepted
			attempt.FocusedTestEvidenceRef = evidenceRef
			next.TestsAdded = len(attempt.ChangedTestPaths)
			next.ChangedTestPaths = append([]string(nil), attempt.ChangedTestPaths...)
			next.PatchDigest = attempt.PatchDigest
			next.LastError = ""
			next.Stage = StageFinalVerify
			break
		}
		diagnostic, err := boundedDiagnostic(event.Diagnostic)
		if err != nil {
			return Run{}, err
		}
		attempt.Status = AttemptFailed
		attempt.FocusedTestEvidenceRef = evidenceRef
		attempt.LastError = diagnostic
		next.LastError = diagnostic
		if len(next.Attempts) >= next.MaxAttempts {
			next.Status = StatusFailed
		} else {
			next.Stage = StageGenerate
		}
	case EventFinalVerifyCompleted:
		if r.Status != StatusRunning || r.Stage != StageFinalVerify || r.TestsAdded == 0 {
			return Run{}, fmt.Errorf("%w: final verification is not expected at this stage", ErrInvalidTransition)
		}
		if !event.Succeeded {
			return failedRun(r, event, "final Maven verify failed")
		}
		evidenceRef, err := boundedValue(event.EvidenceRef, 2048, "verification evidence reference")
		if err != nil || evidenceRef == "" {
			return Run{}, fmt.Errorf("%w: successful final verification requires a report reference", ErrInvalidEvent)
		}
		next.EvidenceRef = evidenceRef
		next.Stage = StageOpenPR
	case EventPRCompleted:
		if r.Status != StatusRunning || r.Stage != StageOpenPR || r.TestsAdded == 0 {
			return Run{}, fmt.Errorf("%w: PR result is not expected at this stage", ErrInvalidTransition)
		}
		prURL, err := boundedURL(event.PRURL, "PR_URL")
		if err != nil {
			return Run{}, err
		}
		if !event.Succeeded {
			return failedRun(r, event, event.Diagnostic)
		}
		if event.EvidenceRef != "" && event.EvidenceRef != r.EvidenceRef {
			return Run{}, fmt.Errorf("%w: PR event evidence differs from the verified report", ErrInvalidEvent)
		}
		next.PRURL = prURL
		next.Stage = StageDone
		next.Status = StatusCompleted
	case EventStageFailed:
		return failedRun(r, event, event.Diagnostic)
	case EventCancelRequested:
		if r.Status != StatusQueued && r.Status != StatusRunning {
			return Run{}, fmt.Errorf("%w: cancellation is not available for %s runs", ErrInvalidTransition, r.Status)
		}
		at := event.OccurredAt.UTC()
		next.CancelRequestedAt = &at
		next.Status = StatusCancelRequested
	case EventCancelled:
		if r.Status != StatusCancelRequested || !event.TerminationConfirmed {
			return Run{}, fmt.Errorf("%w: cancellation requires a pending request and confirmed worker termination", ErrInvalidTransition)
		}
		next.Status = StatusCancelled
	case EventOutcomeUnknown:
		if r.Status != StatusQueued && r.Status != StatusRunning && r.Status != StatusCancelRequested {
			return Run{}, fmt.Errorf("%w: outcome cannot become unknown from %s", ErrInvalidTransition, r.Status)
		}
		diagnostic, err := boundedDiagnostic(event.Diagnostic)
		if err != nil {
			return Run{}, err
		}
		next.LastError = diagnostic
		next.Status = StatusOutcomeUnknown
	default:
		return Run{}, fmt.Errorf("%w: unsupported event type %q", ErrInvalidEvent, event.Type)
	}
	if err := next.Validate(); err != nil {
		return Run{}, err
	}
	return next, nil
}

func (r Run) Validate() error {
	if !validFlowIdent(r.RunID) || !validFlowIdent(r.IdempotencyKey) || strings.TrimSpace(r.IdempotencyKey) != r.IdempotencyKey || !sha256Pattern.MatchString(r.RequestDigest) {
		return fmt.Errorf("%w: run identity or request digest is invalid", ErrInvalidRun)
	}
	if err := r.Request.Validate(); err != nil {
		return err
	}
	requestDigest, err := requestDigest(r.Request, r.MaxAttempts)
	if err != nil || requestDigest != strings.ToLower(r.RequestDigest) {
		return fmt.Errorf("%w: request digest does not bind the immutable request", ErrInvalidRun)
	}
	if !validStatus(r.Status) || !validStage(r.Stage) || r.Revision < 1 || r.MaxAttempts < 1 || r.MaxAttempts > DefaultMaxGenerationAttempts || len(r.Attempts) > r.MaxAttempts {
		return fmt.Errorf("%w: state, revision, or attempt count is invalid", ErrInvalidRun)
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) || r.Attempts == nil || r.ChangedTestPaths == nil {
		return fmt.Errorf("%w: timestamps and collections are invalid", ErrInvalidRun)
	}
	if r.ResolvedCommit != "" && (!commitPattern.MatchString(r.ResolvedCommit) || !strings.EqualFold(r.ResolvedCommit, r.Request.Revision)) {
		return fmt.Errorf("%w: resolved commit differs from immutable request revision", ErrInvalidRun)
	}
	for i, attempt := range r.Attempts {
		if attempt.Number != i+1 || !validAttemptStatus(attempt.Status) || attempt.StartedAt.IsZero() || attempt.StartedAt.Before(r.CreatedAt) {
			return fmt.Errorf("%w: generation attempt identity or timestamps are invalid", ErrInvalidRun)
		}
		if attempt.FinishedAt != nil && attempt.FinishedAt.Before(attempt.StartedAt) {
			return fmt.Errorf("%w: generation attempt finish precedes start", ErrInvalidRun)
		}
		if len(attempt.ChangedTestPaths) > 0 {
			if _, err := validateTestPaths(attempt.ChangedTestPaths); err != nil {
				return err
			}
			if !sha256Pattern.MatchString(attempt.PatchDigest) {
				return fmt.Errorf("%w: generated attempt patch digest is invalid", ErrInvalidRun)
			}
		}
		if len(attempt.LastError) > 1024 {
			return fmt.Errorf("%w: generation error exceeds limit", ErrInvalidRun)
		}
		if _, err := boundedValue(attempt.FocusedTestEvidenceRef, 2048, "focused test evidence reference"); err != nil {
			return fmt.Errorf("%w: focused test evidence reference is invalid", ErrInvalidRun)
		}
		if attempt.Status == AttemptAccepted && attempt.FocusedTestEvidenceRef == "" {
			return fmt.Errorf("%w: accepted generation requires focused test evidence", ErrInvalidRun)
		}
	}
	paths, err := validateTestPaths(r.ChangedTestPaths)
	if err != nil {
		return err
	}
	if r.TestsAdded != len(paths) {
		return fmt.Errorf("%w: tests_added must equal the accepted changed-test file count", ErrInvalidRun)
	}
	if r.TestsAdded > 0 && !sha256Pattern.MatchString(r.PatchDigest) {
		return fmt.Errorf("%w: accepted patch digest is invalid", ErrInvalidRun)
	}
	if r.TestsAdded > 0 && (r.Stage == StageOpenPR || r.Status == StatusCompleted) && r.EvidenceRef == "" {
		return fmt.Errorf("%w: verified tests require a durable report reference before PR or completion", ErrInvalidRun)
	}
	if r.TestsAdded == 0 && (len(paths) != 0 || r.PatchDigest != "" || r.PRURL != "") {
		return fmt.Errorf("%w: zero-test result cannot reference a patch or PR", ErrInvalidRun)
	}
	if _, err := boundedURL(r.PRURL, "PR_URL"); err != nil {
		return err
	}
	if _, err := boundedValue(r.EvidenceRef, 2048, "evidence reference"); err != nil {
		return err
	}
	if len(r.LastError) > 1024 {
		return fmt.Errorf("%w: last error exceeds limit", ErrInvalidRun)
	}
	if r.Status == StatusCompleted && r.Stage != StageDone || r.Status == StatusQueued && r.Stage != StageSource {
		return fmt.Errorf("%w: status and stage disagree", ErrInvalidRun)
	}
	if r.Stage == StageDone && r.Status != StatusCompleted && r.Status != StatusCancelled {
		return fmt.Errorf("%w: only completed or cancelled runs can be at the done stage", ErrInvalidRun)
	}
	if r.CancelRequestedAt != nil && r.CancelRequestedAt.Before(r.CreatedAt) {
		return fmt.Errorf("%w: cancellation timestamp is invalid", ErrInvalidRun)
	}
	return nil
}

func (r Run) AttemptCount() int { return len(r.Attempts) }

func cloneRun(r Run) Run {
	if r.Attempts == nil {
		r.Attempts = []GenerationAttempt{}
	} else {
		r.Attempts = append([]GenerationAttempt{}, r.Attempts...)
	}
	for i := range r.Attempts {
		r.Attempts[i].ChangedTestPaths = append([]string(nil), r.Attempts[i].ChangedTestPaths...)
		if r.Attempts[i].FinishedAt != nil {
			finishedAt := *r.Attempts[i].FinishedAt
			r.Attempts[i].FinishedAt = &finishedAt
		}
	}
	if r.ChangedTestPaths == nil {
		r.ChangedTestPaths = []string{}
	} else {
		r.ChangedTestPaths = append([]string{}, r.ChangedTestPaths...)
	}
	if r.CancelRequestedAt != nil {
		cancelledAt := *r.CancelRequestedAt
		r.CancelRequestedAt = &cancelledAt
	}
	return r
}

func failedRun(r Run, event Event, fallback string) (Run, error) {
	diagnostic := event.Diagnostic
	if strings.TrimSpace(diagnostic) == "" {
		diagnostic = fallback
	}
	diagnostic, err := boundedDiagnostic(diagnostic)
	if err != nil {
		return Run{}, err
	}
	next := cloneRun(r)
	next.Revision++
	next.UpdatedAt = event.OccurredAt.UTC()
	next.Status = StatusFailed
	next.LastError = diagnostic
	if event.EvidenceRef != "" {
		evidenceRef, err := boundedValue(event.EvidenceRef, 2048, "evidence reference")
		if err != nil {
			return Run{}, err
		}
		next.EvidenceRef = evidenceRef
	}
	if len(next.Attempts) > 0 && next.Attempts[len(next.Attempts)-1].Status == AttemptRunning {
		attempt := &next.Attempts[len(next.Attempts)-1]
		attempt.Status = AttemptFailed
		attempt.LastError = diagnostic
		finishedAt := event.OccurredAt.UTC()
		attempt.FinishedAt = &finishedAt
	}
	if err := next.Validate(); err != nil {
		return Run{}, err
	}
	return next, nil
}

func validateTestPaths(values []string) ([]string, error) {
	if len(values) > 1000 {
		return nil, fmt.Errorf("%w: changed test path count exceeds limit", ErrInvalidEvent)
	}
	paths := append([]string(nil), values...)
	seen := make(map[string]struct{}, len(paths))
	for _, value := range paths {
		if value == "" || strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || path.Clean(value) != value || len(value) > 1024 || strings.HasPrefix(value, "../") {
			return nil, fmt.Errorf("%w: changed test path is not normalized and relative", ErrInvalidEvent)
		}
		parts := strings.Split(value, "/")
		underTest := false
		for i := 0; i+2 < len(parts); i++ {
			if parts[i] == "src" && parts[i+1] == "test" {
				underTest = true
				break
			}
		}
		if !underTest || !strings.HasSuffix(value, ".java") {
			return nil, fmt.Errorf("%w: only Java files under a src/test tree are accepted", ErrInvalidEvent)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("%w: changed test paths must be unique", ErrInvalidEvent)
		}
		seen[value] = struct{}{}
	}
	sort.Strings(paths)
	return paths, nil
}

func boundedDiagnostic(value string) (string, error) {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\x00' || r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value))
	if value == "" || len(value) > 1024 {
		return "", fmt.Errorf("%w: diagnostic must be non-empty and at most 1024 bytes", ErrInvalidEvent)
	}
	return value, nil
}

func boundedURL(value, name string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) > 2048 || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%w: %s is invalid or oversized", ErrInvalidRun, name)
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("%w: %s must be a credential-free HTTP URL", ErrInvalidRun, name)
	}
	return value, nil
}

func boundedValue(value string, maximum int, name string) (string, error) {
	if len(value) > maximum || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%w: %s is invalid or oversized", ErrInvalidEvent, name)
	}
	return value, nil
}

func validFlowIdent(value string) bool { return flowIDPattern.MatchString(value) }

func validCommit(value string) bool { return commitPattern.MatchString(value) }

func validBranch(value string) bool {
	return branchNameRegexp.MatchString(value) && value != "@" && !strings.HasPrefix(value, "-") &&
		!strings.HasPrefix(value, "/") && !strings.HasSuffix(value, "/") && !strings.HasSuffix(value, ".") &&
		!strings.Contains(value, "..") && !strings.Contains(value, "//") && !strings.Contains(value, "@{") &&
		!strings.HasSuffix(value, ".lock")
}

func validSubdirectory(value string) bool {
	return value != "" && !strings.ContainsAny(value, "\\\x00") && !path.IsAbs(value) &&
		path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../")
}

func validStatus(value Status) bool {
	switch value {
	case StatusQueued, StatusRunning, StatusCancelRequested, StatusCompleted, StatusFailed, StatusCancelled, StatusOutcomeUnknown:
		return true
	default:
		return false
	}
}

func validStage(value Stage) bool {
	switch value {
	case StageSource, StageVerifyCommit, StageBaseline, StageGenerate, StageFocusedTest, StageFinalVerify, StageOpenPR, StageDone:
		return true
	default:
		return false
	}
}

func validAttemptStatus(value AttemptStatus) bool {
	switch value {
	case AttemptRunning, AttemptGenerated, AttemptAccepted, AttemptNoTests, AttemptFailed:
		return true
	default:
		return false
	}
}

func isActive(status Status) bool { return status == StatusQueued || status == StatusRunning }

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func requestDigest(request Request, maxAttempts int) (string, error) {
	data, err := json.Marshal(struct {
		Request     Request `json:"request"`
		MaxAttempts int     `json:"max_attempts"`
	}{Request: request, MaxAttempts: maxAttempts})
	if err != nil {
		return "", err
	}
	return digest(data), nil
}
