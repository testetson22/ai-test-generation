// Package localservice composes the development-only, real execution flow.
package localservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/agentworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/httpapi"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

type Repository interface {
	testgenflow.Repository
	Ping(context.Context) error
	ClaimNextTestGenerationRun(context.Context, string, string, time.Duration) (*testgenflow.FlowClaim, error)
	RenewTestGenerationClaim(context.Context, testgenflow.FlowClaim, time.Duration) error
	ReleaseTestGenerationClaim(context.Context, testgenflow.FlowClaim) error
	ApplyClaimedTestGenerationEvent(context.Context, testgenflow.FlowClaim, testgenflow.Event) (testgenflow.Run, error)
	BeginTestGenerationOperation(context.Context, testgenflow.FlowClaim, string, string, string) (testgenflow.Operation, bool, error)
	FinishTestGenerationOperation(context.Context, testgenflow.FlowClaim, string, string, json.RawMessage, string) error
	ListTestGenerationOperations(context.Context, string) ([]testgenflow.Operation, error)
}

type Worker interface {
	testgenflow.SourceProvider
	testgenflow.ArtifactInputProvider
	testgenflow.MavenProfileRunner
	Ready(context.Context) error
	CancelRun(context.Context, string) (string, error)
	CleanupRun(context.Context, string) error
	ConfirmOperations(context.Context, string, []string) error
	RunEvidence(context.Context, string) ([]string, error)
}

type Config struct {
	Repository           Repository
	Worker               Worker
	Artifacts            artifact.Store
	API                  httpapi.TestGenerationDevelopmentConfig
	ValidateTestArtifact func([]byte) error
	MaxTestArtifactBytes int64
	PollInterval         time.Duration
	LeaseDuration        time.Duration
	Logger               *slog.Logger
	Generation           GenerationWorker
	AllowModelCalls      bool
	ReviewTTL            time.Duration
}

type Service struct {
	config  Config
	api     *httpapi.TestGenerationDevelopmentAPI
	holder  string
	running atomic.Bool
}

func New(config Config) (*Service, error) {
	if config.Repository == nil || config.Worker == nil || config.Artifacts == nil || config.ValidateTestArtifact == nil {
		return nil, errors.New("local execution requires PostgreSQL, artifact storage, an isolated worker, and artifact validation")
	}
	modelMode := config.API.InputMode == testgenflow.InputModeModelGeneration
	if (!modelMode && config.API.InputMode != testgenflow.InputModeArtifactInput) || config.API.CompletionMode != testgenflow.CompletionModePatchOnly {
		return nil, errors.New("local execution requires explicit artifact_input or model_generation and patch_only policies")
	}
	if modelMode {
		if !config.AllowModelCalls {
			return nil, agentworker.ErrExternalOperationsDisabled
		}
		if config.Generation == nil || config.Generation.PolicyDigest() != config.API.GenerationPolicyDigest {
			return nil, errors.New("model mode requires a ready digest-bound generation worker")
		}
		if _, ok := config.Repository.(modelGenerationClaims); !ok {
			return nil, errors.New("model mode requires policy-scoped durable claims")
		}
	} else if config.Generation != nil || config.AllowModelCalls {
		return nil, errors.New("generation must not be configured for artifact-only mode")
	}
	if config.MaxTestArtifactBytes == 0 {
		config.MaxTestArtifactBytes = 1 << 20
	}
	if config.ReviewTTL == 0 {
		config.ReviewTTL = time.Hour
	}
	if config.ReviewTTL < time.Minute || config.ReviewTTL > 24*time.Hour {
		return nil, errors.New("generation review TTL must be between one minute and one day")
	}
	if config.PollInterval == 0 {
		config.PollInterval = 250 * time.Millisecond
	}
	if config.LeaseDuration == 0 {
		config.LeaseDuration = 15 * time.Second
	}
	if config.PollInterval < 10*time.Millisecond || config.LeaseDuration < 200*time.Millisecond {
		return nil, errors.New("local driver polling or lease interval is too short")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	api := &httpapi.TestGenerationDevelopmentAPI{
		Repository: config.Repository, Config: config.API, Artifacts: config.Artifacts,
		ValidateTestArtifact: config.ValidateTestArtifact, MaxTestArtifactBytes: config.MaxTestArtifactBytes,
		Operations: config.Repository.ListTestGenerationOperations,
	}
	service := &Service{config: config, api: api}
	if modelMode {
		api.ApproveGeneratedCode = service.approveGeneratedCode
		api.ReconcileGeneration = service.reconcileUnknownPlan
	}
	if !api.Ready() {
		return nil, errors.New("local execution API configuration is invalid")
	}
	holder, err := testgenflow.NewRunID()
	if err != nil {
		return nil, err
	}
	service.holder = holder
	return service, nil
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	s.api.Register(mux)
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		code, state := http.StatusOK, "ready"
		if !s.running.Load() || s.Ready(ctx) != nil {
			code, state = http.StatusServiceUnavailable, "unavailable"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": state, "mode": "local-development", "input_mode": s.config.API.InputMode,
			"completion_mode": "patch_only", "live_generation": s.config.API.InputMode == testgenflow.InputModeModelGeneration, "production_ready": false,
		})
	})
	mux.Handle("/", httpapi.NewHandler(httpapi.Dependencies{}))
	return mux
}

func (s *Service) Ready(ctx context.Context) error {
	if err := s.config.Repository.Ping(ctx); err != nil {
		return fmt.Errorf("local state store unavailable: %w", err)
	}
	if err := s.config.Worker.Ready(ctx); err != nil {
		return fmt.Errorf("local worker unavailable: %w", err)
	}
	if s.config.Generation != nil {
		if !s.config.AllowModelCalls {
			return agentworker.ErrExternalOperationsDisabled
		}
		if err := s.config.Generation.Ready(ctx); err != nil {
			return fmt.Errorf("generation worker unavailable: %w", err)
		}
	}
	canary := []byte("atg-local-development-artifact-readiness-v1\n")
	ref, err := s.config.Artifacts.Put(ctx, canary)
	if err != nil {
		return err
	}
	actual, err := s.config.Artifacts.Get(ctx, ref.Digest, 1024)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, canary) {
		return artifact.ErrCorrupt
	}
	return nil
}

// Run owns sequencing only; real workers are reconciled through durable operation
// identities. Stopping this process does not authorize a fresh external attempt.
func (s *Service) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("local driver is already running")
	}
	defer s.running.Store(false)
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		worked, err := s.DriveOnce(ctx)
		if err != nil && ctx.Err() == nil {
			s.config.Logger.Error("local flow reconciliation failed", "error", err)
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) DriveOnce(ctx context.Context) (bool, error) {
	var claim *testgenflow.FlowClaim
	var err error
	if s.config.API.InputMode == testgenflow.InputModeModelGeneration {
		claim, err = s.config.Repository.(modelGenerationClaims).ClaimNextModelGenerationRun(ctx, s.holder, s.config.API.ProfileDigest, s.config.API.GenerationPolicyDigest, s.config.LeaseDuration)
	} else {
		claim, err = s.config.Repository.ClaimNextTestGenerationRun(ctx, s.holder, s.config.API.ProfileDigest, s.config.LeaseDuration)
	}
	if err != nil || claim == nil {
		return false, err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = s.config.Repository.ReleaseTestGenerationClaim(releaseCtx, *claim)
	}()
	run, err := s.config.Repository.GetRun(ctx, claim.RunID)
	if err != nil {
		return true, err
	}
	if run.Request.InputMode != s.config.API.InputMode || run.Request.CompletionMode != testgenflow.CompletionModePatchOnly || (run.Request.InputMode == testgenflow.InputModeModelGeneration && (run.Request.Generation == nil || run.Request.Generation.PolicyDigest != s.config.API.GenerationPolicyDigest)) {
		return true, errors.New("local driver refused a non-local run")
	}
	workCtx, cancel := context.WithCancel(ctx)
	heartbeatStop, heartbeatDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(s.config.LeaseDuration / 4)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := s.config.Repository.RenewTestGenerationClaim(workCtx, *claim, s.config.LeaseDuration); err != nil {
					cancel()
					return
				}
				latest, err := s.config.Repository.GetRun(workCtx, claim.RunID)
				if err != nil || (latest.Status == testgenflow.StatusCancelRequested && run.Status != testgenflow.StatusCancelRequested) {
					cancel()
					return
				}
			}
		}
	}()
	workers := &operations{service: s, claim: *claim}
	if run.Status == testgenflow.StatusCancelRequested {
		err = workers.cancel(workCtx, run)
	} else {
		coordinator := testgenflow.Coordinator{
			Repository: &claimedRepository{Repository: s.config.Repository, claim: *claim},
			Source:     workers, ArtifactInput: workers, Maven: workers, Publisher: workers,
			Artifacts: s.config.Artifacts,
			Now:       s.now,
		}
		if s.config.Generation != nil {
			coordinator.Generation = workers
		}
		_, err = coordinator.Advance(workCtx, run.RunID)
	}
	close(heartbeatStop)
	cancel()
	<-heartbeatDone
	if errors.Is(err, testgenflow.ErrWorkPending) {
		return false, nil
	}
	if errors.Is(err, testgenflow.ErrRevisionConflict) || errors.Is(err, testgenflow.ErrFlowClaimLost) || (errors.Is(err, context.Canceled) && ctx.Err() == nil) {
		return true, nil
	}
	return true, err
}

type claimedRepository struct {
	Repository
	claim testgenflow.FlowClaim
}

func (r *claimedRepository) ApplyEvent(ctx context.Context, runID string, event testgenflow.Event) (testgenflow.Run, error) {
	if runID != r.claim.RunID {
		return testgenflow.Run{}, testgenflow.ErrFlowClaimLost
	}
	return r.ApplyClaimedTestGenerationEvent(ctx, r.claim, event)
}
