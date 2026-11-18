package tekton

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

var (
	ErrAlreadyExists      = errors.New("Tekton run already exists")
	ErrNotFound           = errors.New("Tekton run not found")
	ErrCreateAckLost      = errors.New("Tekton run create acknowledgement was lost")
	ErrForbidden          = errors.New("Tekton API request is forbidden")
	ErrInvalidResource    = errors.New("Tekton rejected the run resource")
	ErrCleanupNotReady    = errors.New("Tekton cleanup is not yet safe")
	ErrCleanupPending     = errors.New("Tekton cleanup is pending")
	ErrEvidenceNotDurable = errors.New("required evidence is not durably acknowledged")
)

// API is the narrow Tekton API boundary. Implementations must use UID and
// resourceVersion preconditions for cancellation/deletion. DeleteRun may return
// confirmed=true only after the run and its owned workload resources are gone.
type API interface {
	CreateRun(context.Context, Resource) (Resource, error)
	GetRun(context.Context, string, RunKind, string) (Resource, error)
	RequestCancellation(context.Context, Resource) (Resource, error)
	DeleteRun(context.Context, Resource) (confirmed bool, err error)
}

type EvidenceGate interface {
	Durable(context.Context, provider.SubmitRequest) (bool, error)
}

type Clock interface {
	Now() time.Time
}

type Provider struct {
	config   Config
	api      API
	evidence EvidenceGate
	clock    Clock
}

func NewProvider(config Config, api API, evidence EvidenceGate, clock Clock) (*Provider, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if api == nil || evidence == nil {
		return nil, fmt.Errorf("Tekton API and durable evidence gates are required")
	}
	if clock == nil {
		return nil, fmt.Errorf("Tekton provider clock is required")
	}
	return &Provider{config: config, api: api, evidence: evidence, clock: clock}, nil
}

func (p *Provider) Submit(ctx context.Context, request provider.SubmitRequest) (provider.JobRef, error) {
	expected, err := Render(p.config, request)
	if err != nil {
		return provider.JobRef{}, err
	}
	created, err := p.api.CreateRun(ctx, expected)
	if errors.Is(err, ErrAlreadyExists) {
		created, err = p.api.GetRun(ctx, p.config.Namespace, p.config.Kind, expected.Metadata.Name)
		if errors.Is(err, ErrNotFound) {
			return provider.JobRef{}, &provider.SubmissionError{Category: "provider_unavailable", OutcomeUnknown: true, Err: err}
		}
		if err != nil {
			return provider.JobRef{}, err
		}
	} else if err != nil {
		if errors.Is(err, ErrCreateAckLost) {
			return provider.JobRef{}, &provider.SubmissionError{Category: "provider_unavailable", OutcomeUnknown: true, Err: err}
		}
		if errors.Is(err, ErrForbidden) {
			return provider.JobRef{}, &provider.SubmissionError{Category: "unauthorized", Err: err}
		}
		if errors.Is(err, ErrInvalidResource) {
			return provider.JobRef{}, &provider.SubmissionError{Category: "invalid_input", Err: err}
		}
		return provider.JobRef{}, &provider.SubmissionError{Category: "provider_unavailable", OutcomeUnknown: true, Err: err}
	}
	if err := VerifyResourceIdentity(expected, created); err != nil {
		return provider.JobRef{}, err
	}
	return jobReference(created), nil
}

func (p *Provider) Observe(ctx context.Context, request provider.SubmitRequest) (provider.Observation, error) {
	expected, err := Render(p.config, request)
	if err != nil {
		return provider.Observation{}, err
	}
	resource, err := p.api.GetRun(ctx, p.config.Namespace, p.config.Kind, expected.Metadata.Name)
	if errors.Is(err, ErrNotFound) {
		return provider.Observation{
			SubmissionToken: request.SubmissionToken, TaskID: request.TaskID, AttemptID: request.AttemptID,
			TaskSpecDigest: request.TaskSpecDigest, ProfileID: request.ProfileID,
			ProfileVersion: request.ProfileVersion, ProfileDigest: request.ProfileDigest,
			Status: provider.StatusNotFound, ObservedAt: p.clock.Now().UTC(),
		}, nil
	}
	if err != nil {
		return provider.Observation{}, err
	}
	return ObservationFromResource(expected, request, resource, p.clock.Now())
}

func (p *Provider) Cancel(ctx context.Context, request provider.SubmitRequest) (provider.Observation, error) {
	expected, err := Render(p.config, request)
	if err != nil {
		return provider.Observation{}, err
	}
	resource, err := p.api.GetRun(ctx, p.config.Namespace, p.config.Kind, expected.Metadata.Name)
	if errors.Is(err, ErrNotFound) {
		return provider.Observation{
			SubmissionToken: request.SubmissionToken, TaskID: request.TaskID, AttemptID: request.AttemptID,
			TaskSpecDigest: request.TaskSpecDigest, ProfileID: request.ProfileID,
			ProfileVersion: request.ProfileVersion, ProfileDigest: request.ProfileDigest,
			Status: provider.StatusNotFound, ObservedAt: p.clock.Now().UTC(),
		}, nil
	}
	if err != nil {
		return provider.Observation{}, err
	}
	current, err := ObservationFromResource(expected, request, resource, p.clock.Now())
	if err != nil {
		return provider.Observation{}, err
	}
	if current.Status == provider.StatusCompleted || current.Status == provider.StatusFailed || current.Status == provider.StatusCancelled || current.Status == provider.StatusCancelRequested {
		return current, nil
	}
	updated, err := p.api.RequestCancellation(ctx, resource)
	if err != nil {
		return provider.Observation{}, err
	}
	observation, err := ObservationFromResource(expected, request, updated, p.clock.Now())
	if err != nil {
		return provider.Observation{}, err
	}
	if observation.Status == provider.StatusPending || observation.Status == provider.StatusRunning || observation.Status == provider.StatusOutcomeUnknown {
		observation.Status = provider.StatusCancelRequested
		observation.TerminationConfirmed = false
	}
	return observation, nil
}

func (p *Provider) Cleanup(ctx context.Context, request provider.SubmitRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	durable, err := p.evidence.Durable(ctx, request)
	if err != nil {
		return err
	}
	if !durable {
		return ErrEvidenceNotDurable
	}
	expected, err := Render(p.config, request)
	if err != nil {
		return err
	}
	resource, err := p.api.GetRun(ctx, p.config.Namespace, p.config.Kind, expected.Metadata.Name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	observation, err := ObservationFromResource(expected, request, resource, p.clock.Now())
	if err != nil {
		return err
	}
	if !observation.TerminationConfirmed || observation.Status != provider.StatusCompleted && observation.Status != provider.StatusFailed && observation.Status != provider.StatusCancelled {
		return ErrCleanupNotReady
	}
	confirmed, err := p.api.DeleteRun(ctx, resource)
	if err != nil {
		return err
	}
	if !confirmed {
		return ErrCleanupPending
	}
	return nil
}

func jobReference(resource Resource) provider.JobRef {
	return provider.JobRef{ID: resource.Metadata.Namespace + "/" + resource.Metadata.Name + ":" + resource.Metadata.UID}
}
