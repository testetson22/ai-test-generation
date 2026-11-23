package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/agentworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

func TestGenerationSettingsDefaultRemainsArtifactOnly(t *testing.T) {
	settings, err := loadGenerationSettings(func(name string) string {
		if name != "ATG_LOCAL_INPUT_MODE" {
			t.Fatalf("artifact-only settings read generation option %s", name)
		}
		return ""
	})
	if err != nil || settings.enabled || settings.mode != testgenflow.InputModeArtifactInput || settings.attempts != 1 {
		t.Fatalf("unexpected default: %#v %v", settings, err)
	}
}

func TestModelPermissionGatePrecedesDependenciesAndCredentialAccess(t *testing.T) {
	for _, permit := range []string{"", "false", "1", "TRUE"} {
		t.Run(permit, func(t *testing.T) {
			getenv := func(name string) string {
				switch name {
				case "ATG_LOCAL_INPUT_MODE":
					return testgenflow.InputModeModelGeneration
				case "ATG_ALLOW_MODEL_CALLS":
					return permit
				default:
					t.Fatalf("permission-closed startup read %s", name)
					return ""
				}
			}
			if _, err := loadGenerationSettings(getenv); !errors.Is(err, agentworker.ErrExternalOperationsDisabled) {
				t.Fatalf("settings permission gate: %v", err)
			}
			if _, err := newLocalRuntime(t.Context(), getenv, nil); !errors.Is(err, agentworker.ErrExternalOperationsDisabled) {
				t.Fatalf("runtime bypassed permission gate: %v", err)
			}
		})
	}
}

func TestModelPermissionGateDoesNotCreateArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-be-created")
	_, err := newLocalRuntime(t.Context(), func(name string) string {
		switch name {
		case "ATG_LOCAL_INPUT_MODE":
			return testgenflow.InputModeModelGeneration
		case "ATG_ARTIFACT_ROOT":
			return root
		default:
			return ""
		}
	}, nil)
	if !errors.Is(err, agentworker.ErrExternalOperationsDisabled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("closed model mode touched artifact storage: %v", err)
	}
}

func completeGenerationSettings() map[string]string {
	return map[string]string{
		"ATG_LOCAL_INPUT_MODE": testgenflow.InputModeModelGeneration, "ATG_ALLOW_MODEL_CALLS": "true",
		"ATG_GENERATION_POLICY": "not-read-policy.json", "ATG_GENERATION_IMAGE": "localhost/generation:unit",
		"ATG_GENERATION_CREDENTIAL_FILE": "not-read-credential", "ATG_DEVELOPMENT_REVIEWER_ID": "unit-reviewer",
		"ATG_DEVELOPMENT_REVIEWER_ROLE": "reviewer", "ATG_DEVELOPMENT_REVIEW_AUTHORIZATION_REF": "authz://unit-test",
	}
}

func TestEnabledGenerationSettingsRequireExplicitPolicyAndReviewer(t *testing.T) {
	settings := completeGenerationSettings()
	get := func(key string) string { return settings[key] }
	actual, err := loadGenerationSettings(get)
	if err != nil || !actual.enabled || actual.attempts != 2 || actual.reviewTTL != time.Hour || actual.reviewer.ReviewerID != "unit-reviewer" {
		t.Fatalf("settings: %#v %v", actual, err)
	}
	for _, key := range []string{"ATG_GENERATION_POLICY", "ATG_GENERATION_IMAGE", "ATG_GENERATION_CREDENTIAL_FILE", "ATG_DEVELOPMENT_REVIEWER_ID", "ATG_DEVELOPMENT_REVIEWER_ROLE", "ATG_DEVELOPMENT_REVIEW_AUTHORIZATION_REF"} {
		t.Run(key, func(t *testing.T) {
			old := settings[key]
			settings[key] = ""
			defer func() { settings[key] = old }()
			if _, err := loadGenerationSettings(get); err == nil {
				t.Fatalf("accepted missing %s", key)
			}
		})
	}
	for key, values := range map[string][]string{
		"ATG_GENERATION_MAX_ATTEMPTS": {"0", "7", "invalid"}, "ATG_GENERATION_REVIEW_TTL": {"0s", "25h", "invalid"},
		"ATG_DEVELOPMENT_REVIEWER_ID": {"actor with spaces", "actor\n"}, "ATG_DEVELOPMENT_REVIEWER_ROLE": {"admin", "model"},
	} {
		for _, value := range values {
			t.Run(key+value, func(t *testing.T) {
				old := settings[key]
				settings[key] = value
				defer func() { settings[key] = old }()
				if _, err := loadGenerationSettings(get); err == nil {
					t.Fatalf("accepted invalid %s", key)
				}
			})
		}
	}
	settings["ATG_LOCAL_INPUT_MODE"] = "simulation-as-model"
	if _, err := loadGenerationSettings(get); err == nil || !strings.Contains(err.Error(), "ATG_LOCAL_INPUT_MODE") {
		t.Fatalf("accepted unknown mode: %v", err)
	}
}
