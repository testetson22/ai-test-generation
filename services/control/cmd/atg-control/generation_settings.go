package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/agentworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/httpapi"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

type generationSettings struct {
	mode           string
	enabled        bool
	policyFile     string
	image          string
	credentialFile string
	reviewer       httpapi.TestGenerationDevelopmentConfig
	attempts       int
	reviewTTL      time.Duration
}

// Parse the opt-in before opening state, reading credentials or probing engines.
// Merely selecting model mode never authorizes an external operation.
func loadGenerationSettings(getenv func(string) string) (generationSettings, error) {
	s := generationSettings{mode: getenv("ATG_LOCAL_INPUT_MODE"), attempts: 1, reviewTTL: time.Hour}
	if s.mode == "" {
		s.mode = testgenflow.InputModeArtifactInput
	}
	if s.mode == testgenflow.InputModeArtifactInput {
		return s, nil
	}
	if s.mode != testgenflow.InputModeModelGeneration {
		return s, fmt.Errorf("unsupported ATG_LOCAL_INPUT_MODE")
	}
	if getenv("ATG_ALLOW_MODEL_CALLS") != "true" {
		return s, agentworker.ErrExternalOperationsDisabled
	}
	s.enabled = true
	s.policyFile = getenv("ATG_GENERATION_POLICY")
	s.image = getenv("ATG_GENERATION_IMAGE")
	s.credentialFile = getenv("ATG_GENERATION_CREDENTIAL_FILE")
	for key, value := range map[string]string{"ATG_GENERATION_POLICY": s.policyFile, "ATG_GENERATION_IMAGE": s.image, "ATG_GENERATION_CREDENTIAL_FILE": s.credentialFile} {
		if strings.TrimSpace(value) == "" {
			return s, fmt.Errorf("model mode requires %s", key)
		}
	}
	s.reviewer.ReviewerID = getenv("ATG_DEVELOPMENT_REVIEWER_ID")
	s.reviewer.ReviewerRole = getenv("ATG_DEVELOPMENT_REVIEWER_ROLE")
	s.reviewer.ReviewAuthorizationRef = getenv("ATG_DEVELOPMENT_REVIEW_AUTHORIZATION_REF")
	s.reviewer.ReviewGateMode = getenv("ATG_GENERATION_REVIEW_GATE_MODE")
	if !httpapi.ValidDevelopmentReviewer(s.reviewer) {
		return s, fmt.Errorf("model mode requires a configured reviewer identity, developer/reviewer role, authorization reference, and a manual or automatic review gate mode")
	}
	s.attempts = 2
	if raw := getenv("ATG_GENERATION_MAX_ATTEMPTS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > testgenflow.DefaultMaxGenerationAttempts {
			return s, fmt.Errorf("invalid ATG_GENERATION_MAX_ATTEMPTS")
		}
		s.attempts = n
	}
	if raw := getenv("ATG_GENERATION_REVIEW_TTL"); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil || ttl < time.Minute || ttl > 24*time.Hour {
			return s, fmt.Errorf("invalid ATG_GENERATION_REVIEW_TTL")
		}
		s.reviewTTL = ttl
	}
	return s, nil
}
