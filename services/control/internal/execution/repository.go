package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

var (
	ErrIdempotencyConflict = errors.New("idempotency key is bound to different request bytes")
	ErrProviderChanged     = errors.New("execution provider changed for an existing attempt")
	ErrPairNotFound        = errors.New("execution pair not found")
	ErrStoredTaskCorrupt   = errors.New("persisted task specification digest mismatch")
	ErrStaleRevision       = errors.New("task revision is stale")
	ErrStaleClaim          = errors.New("attempt claim fencing token is stale")
	ErrRetryNotAuthorized  = errors.New("retry is not authorized by current task policy")
	ErrDeadlineExceeded    = errors.New("task deadline has elapsed")
	ErrTaskNotFound        = errors.New("execution task not found")
	ErrResultConflict      = errors.New("execution result conflicts with persisted delivery")
	ErrResultNotReady      = errors.New("execution attempt is not ready to accept a result")
)

type PairInput struct {
	IdempotencyKey string
	BaseSpec       []byte
	CandidateSpec  []byte
	CreatedAt      time.Time
}

func (i PairInput) RequestDigest() string {
	hash := sha256.New()
	for _, body := range [][]byte{i.BaseSpec, i.CandidateSpec} {
		length := uint64(len(body))
		for shift := 56; shift >= 0; shift -= 8 {
			_, _ = hash.Write([]byte{byte(length >> shift)})
		}
		_, _ = hash.Write(body)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

type PairRecord struct {
	PairID          string
	RunID           string
	GenerationID    string
	BaseTaskID      string
	CandidateTaskID string
	RequestDigest   string
	CreatedAt       time.Time
}

type TaskRecord struct {
	TaskID              string
	RunID               string
	PairID              string
	SubjectRole         string
	Specification       []byte
	SpecificationDigest string
	Status              string
	Revision            int64
	AttemptCount        int
	ActiveAttemptID     string
	NextActionAt        time.Time
	TerminalReason      string
	CancelRequestedAt   *time.Time
	ExecutionResult     *Artifact
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type ClaimAction string

const (
	ActionSubmit  ClaimAction = "submit"
	ActionObserve ClaimAction = "observe"
	ActionCancel  ClaimAction = "cancel"
)

type RetryAuthorization struct {
	DecisionID      string
	PolicyID        string
	FailureCategory string
	AuthorizedAt    time.Time
}

type Claim struct {
	TaskID             string
	PairID             string
	RunID              string
	AttemptID          string
	AttemptNumber      int
	TaskSpecDigest     string
	TaskSpec           []byte
	SubmissionToken    string
	Holder             string
	Action             ClaimAction
	FencingToken       int64
	LeaseExpiresAt     time.Time
	RetryAuthorization *RetryAuthorization
	Provider           provider.Identity
	ProviderJob        provider.JobRef
}

type RetryDecision struct {
	DecisionID      string
	TaskID          string
	PredecessorID   string
	FailureCategory string
	PolicyID        string
	AuthorizedAt    time.Time
	NextAttemptAt   time.Time
	AttemptNumber   int
}

type Repository interface {
	CreatePair(context.Context, PairInput) (PairRecord, bool, error)
	GetPair(context.Context, string) (PairRecord, error)
	GetTask(context.Context, string) (TaskRecord, error)
	ScheduleRetryableFailures(context.Context, time.Time) error
	ClaimNext(context.Context, string, provider.Identity, time.Time, time.Duration, time.Duration) (*Claim, error)
	RecordSubmitted(context.Context, Claim, provider.JobRef, time.Time, time.Duration) error
	RecordSubmissionUncertain(context.Context, Claim, time.Time, time.Duration) error
	RecordSubmitFailure(context.Context, Claim, string, time.Time) error
	ScheduleReconciliation(context.Context, Claim, time.Time, time.Duration) error
	BlockClaim(context.Context, Claim, string, string, time.Time) error
	RecordObservation(context.Context, Claim, provider.Observation, time.Time, time.Duration) error
	AuthorizeRetry(context.Context, string, string, time.Time) (RetryDecision, error)
	RequestCancel(context.Context, string, int64, time.Time) (TaskRecord, error)
}
