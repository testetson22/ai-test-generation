package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/agentworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/httpapi"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/localservice"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/localworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/store/postgres"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

type localRuntime struct {
	service   *localservice.Service
	store     *postgres.Store
	artifacts *artifact.FileStore
}

func newLocalRuntime(ctx context.Context, getenv func(string) string, logger *slog.Logger) (*localRuntime, error) {
	settings, err := loadGenerationSettings(getenv)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, key := range []string{"ATG_DATABASE_URL", "ATG_ARTIFACT_ROOT", "ATG_LOCAL_PROFILE", "ATG_DEVELOPMENT_BEARER_TOKEN"} {
		if strings.TrimSpace(getenv(key)) == "" {
			return nil, fmt.Errorf("local-development requires %s", key)
		}
	}
	token := getenv("ATG_DEVELOPMENT_BEARER_TOKEN")
	if len(token) < 24 || strings.ContainsAny(token, "\x00\r\n") {
		return nil, errors.New("development bearer token must contain at least 24 characters and no line breaks")
	}
	profile, err := localworker.LoadProfile(getenv("ATG_LOCAL_PROFILE"))
	if err != nil {
		return nil, fmt.Errorf("load local development profile: %w", err)
	}
	var generationPolicy agentworker.Policy
	if settings.enabled {
		generationPolicy, err = agentworker.LoadPolicy(settings.policyFile)
		if err != nil {
			return nil, fmt.Errorf("load generation policy: %w", err)
		}
		if strings.Contains(generationPolicy.Model.Endpoint, ".invalid") {
			return nil, errors.New("replace the example model endpoint before enabling external operations")
		}
	}
	files, err := artifact.NewFileStore(getenv("ATG_ARTIFACT_ROOT"), httpapi.MaxDevelopmentArtifactBytes)
	if err != nil {
		return nil, fmt.Errorf("open local development artifacts: %w", err)
	}
	worker, err := localworker.New(localworker.Config{Engine: getenv("ATG_LOCAL_ENGINE"), Image: getenv("ATG_LOCAL_IMAGE"), Profile: profile, Artifacts: files})
	if err == nil {
		err = worker.Ready(ctx)
	}
	if err != nil {
		files.Close()
		return nil, fmt.Errorf("local OCI worker is unavailable (build the shared worker image first): %w", err)
	}
	var generation localservice.GenerationWorker
	apiConfig := httpapi.TestGenerationDevelopmentConfig{
		BearerToken: token, RepositoryURL: profile.RepositoryURL, PinnedRevision: profile.Commit,
		DefaultBaseBranch: "main", Subdirectory: profile.Subdirectory, ProfileID: profile.ID,
		ProfileVersion: profile.Version, ProfileDigest: worker.ProfileDigest(), MaxAttempts: settings.attempts,
		InputMode: settings.mode, CompletionMode: testgenflow.CompletionModePatchOnly,
	}
	if settings.enabled {
		generator, err := agentworker.New(agentworker.Config{Engine: getenv("ATG_LOCAL_ENGINE"), Image: settings.image, Policy: generationPolicy, Artifacts: files, SourceEvidence: worker.RunEvidence, CredentialFile: settings.credentialFile, AllowModelCalls: true})
		if err == nil {
			err = generator.Ready(ctx)
		}
		if err != nil {
			files.Close()
			return nil, fmt.Errorf("generation worker is unavailable; external verification is required: %w", err)
		}
		generation = generator
		apiConfig.GenerationPolicyDigest = generator.PolicyDigest()
		apiConfig.GenerationContextPaths = append([]string(nil), generationPolicy.ContextPaths...)
		apiConfig.ReviewerID = settings.reviewer.ReviewerID
		apiConfig.ReviewerRole = settings.reviewer.ReviewerRole
		apiConfig.ReviewAuthorizationRef = settings.reviewer.ReviewAuthorizationRef
	}
	store, err := postgres.Open(ctx, getenv("ATG_DATABASE_URL"))
	if err != nil {
		files.Close()
		return nil, errors.New("local PostgreSQL is unavailable; check ATG_DATABASE_URL and database migrations")
	}
	service, err := localservice.New(localservice.Config{
		Repository: store, Worker: worker, Artifacts: files, Logger: logger,
		MaxTestArtifactBytes: localworker.MaxTestArtifactBytes,
		ValidateTestArtifact: func(raw []byte) error { _, err := localworker.DecodeTestArtifact(raw); return err },
		API:                  apiConfig, Generation: generation, AllowModelCalls: settings.enabled, ReviewTTL: settings.reviewTTL,
	})
	if err != nil {
		store.Close()
		files.Close()
		return nil, err
	}
	if err := service.Ready(ctx); err != nil {
		store.Close()
		files.Close()
		return nil, fmt.Errorf("local execution dependencies are not ready: %w", err)
	}
	logger.Info("local development execution configured", "profile_id", profile.ID, "profile_digest", worker.ProfileDigest(), "live_generation", settings.enabled, "publication", "patch_only")
	return &localRuntime{service: service, store: store, artifacts: files}, nil
}

func (r *localRuntime) Close() {
	r.store.Close()
	r.artifacts.Close()
}
