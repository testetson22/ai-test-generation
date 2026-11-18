package testkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

var ErrCleanupInjected = errors.New("simulated provider cleanup failure")

type simulatedJob struct {
	request     provider.SubmitRequest
	observation provider.Observation
	cleaned     bool
}

// StatefulProvider models an external provider whose state is independent of
// controller process/repository state. Keep the instance while reconstructing a
// controller to test reconciliation after restart.
type StatefulProvider struct {
	mu                   sync.Mutex
	clock                Clock
	jobs                 map[string]*simulatedJob
	loseNextSubmitAck    bool
	deferNextTermination bool
	failNextCleanup      bool
	launchCount          int
}

func NewStatefulProvider(clock Clock) *StatefulProvider {
	if clock == nil {
		clock = wallClock{}
	}
	return &StatefulProvider{clock: clock, jobs: make(map[string]*simulatedJob)}
}

func (p *StatefulProvider) LoseNextSubmitAcknowledgement() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loseNextSubmitAck = true
}

func (p *StatefulProvider) DeferNextTermination() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deferNextTermination = true
}

func (p *StatefulProvider) FailNextCleanup() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failNextCleanup = true
}

func (p *StatefulProvider) Submit(ctx context.Context, request provider.SubmitRequest) (provider.JobRef, error) {
	if err := ctx.Err(); err != nil {
		return provider.JobRef{}, err
	}
	if err := request.Validate(); err != nil {
		return provider.JobRef{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.jobs[request.SubmissionToken]; ok {
		if existing.request != request {
			return provider.JobRef{}, provider.ErrAttemptConflict
		}
		return existing.observation.Job, nil
	}

	p.launchCount++
	job := provider.JobRef{ID: fmt.Sprintf("sim-job-%s", shortToken(request.SubmissionToken))}
	p.jobs[request.SubmissionToken] = &simulatedJob{
		request: request,
		observation: provider.Observation{
			SubmissionToken: request.SubmissionToken,
			TaskID:          request.TaskID, AttemptID: request.AttemptID,
			TaskSpecDigest: request.TaskSpecDigest, ProfileID: request.ProfileID,
			ProfileVersion: request.ProfileVersion, ProfileDigest: request.ProfileDigest,
			Job:        job,
			Sequence:   1,
			Status:     provider.StatusPending,
			ObservedAt: p.clock.Now(),
		},
	}
	if p.loseNextSubmitAck {
		p.loseNextSubmitAck = false
		return provider.JobRef{}, provider.ErrSubmissionAckLost
	}
	return job, nil
}

func (p *StatefulProvider) Observe(ctx context.Context, request provider.SubmitRequest) (provider.Observation, error) {
	if err := ctx.Err(); err != nil {
		return provider.Observation{}, err
	}
	if err := request.Validate(); err != nil {
		return provider.Observation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	job, ok := p.jobs[request.SubmissionToken]
	if !ok {
		return observationForRequest(request, provider.Observation{Status: provider.StatusNotFound, Sequence: 1, ObservedAt: p.clock.Now()}), nil
	}
	if job.request != request {
		return provider.Observation{}, provider.ErrAttemptConflict
	}
	return job.observation, nil
}

func (p *StatefulProvider) Cancel(ctx context.Context, request provider.SubmitRequest) (provider.Observation, error) {
	if err := ctx.Err(); err != nil {
		return provider.Observation{}, err
	}
	if err := request.Validate(); err != nil {
		return provider.Observation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	job, ok := p.jobs[request.SubmissionToken]
	if !ok {
		return provider.Observation{}, provider.ErrAttemptNotFound
	}
	if job.request != request {
		return provider.Observation{}, provider.ErrAttemptConflict
	}
	if job.observation.Status == provider.StatusCancelled || job.observation.Status == provider.StatusCompleted || job.observation.Status == provider.StatusFailed {
		return job.observation, nil
	}
	status := provider.StatusCancelled
	terminated := true
	if p.deferNextTermination {
		p.deferNextTermination = false
		status = provider.StatusCancelRequested
		terminated = false
	}
	return p.setObservationLocked(request.SubmissionToken, status, "", terminated), nil
}

func (p *StatefulProvider) Cleanup(ctx context.Context, request provider.SubmitRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := request.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	job, ok := p.jobs[request.SubmissionToken]
	if !ok {
		return provider.ErrAttemptNotFound
	}
	if job.request != request {
		return provider.ErrAttemptConflict
	}
	if p.failNextCleanup {
		p.failNextCleanup = false
		return ErrCleanupInjected
	}
	job.cleaned = true
	return nil
}

func (p *StatefulProvider) SetStatus(token string, status provider.Status, failure string) (provider.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.jobs[token]; !ok {
		return provider.Observation{}, provider.ErrAttemptNotFound
	}
	return p.setObservationLocked(token, status, failure, status == provider.StatusCancelled), nil
}

func (p *StatefulProvider) ConfirmTermination(token string) (provider.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	job, ok := p.jobs[token]
	if !ok {
		return provider.Observation{}, provider.ErrAttemptNotFound
	}
	if job.observation.Status != provider.StatusCancelRequested {
		return job.observation, nil
	}
	return p.setObservationLocked(token, provider.StatusCancelled, "", true), nil
}

func (p *StatefulProvider) LaunchCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.launchCount
}

func (p *StatefulProvider) WasCleaned(token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	job, ok := p.jobs[token]
	return ok && job.cleaned
}

func (p *StatefulProvider) setObservationLocked(token string, status provider.Status, failure string, terminated bool) provider.Observation {
	job := p.jobs[token]
	job.observation.Sequence++
	job.observation.Status = status
	job.observation.FailureCategory = failure
	job.observation.TerminationConfirmed = terminated
	job.observation.ObservedAt = p.clock.Now()
	return job.observation
}

func observationForRequest(request provider.SubmitRequest, observation provider.Observation) provider.Observation {
	observation.SubmissionToken = request.SubmissionToken
	observation.TaskID = request.TaskID
	observation.AttemptID = request.AttemptID
	observation.TaskSpecDigest = request.TaskSpecDigest
	observation.ProfileID = request.ProfileID
	observation.ProfileVersion = request.ProfileVersion
	observation.ProfileDigest = request.ProfileDigest
	return observation
}

func shortToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}
