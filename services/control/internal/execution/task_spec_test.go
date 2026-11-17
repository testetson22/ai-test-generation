package execution

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	manifestDigest  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	patchDigest     = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testsDigest     = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	profileDigest   = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	baseDigest      = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	candidateDigest = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
)

func TestDecodeTaskSpecRejectsUnknownFieldsAndOversizedInput(t *testing.T) {
	spec := validTaskSpec()
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal valid task spec: %v", err)
	}
	if _, err := DecodeTaskSpec(encoded); err != nil {
		t.Fatalf("decode valid task spec: %v", err)
	}

	unknownField := strings.TrimSuffix(string(encoded), "}") + `,"unknown":true}`
	if _, err := DecodeTaskSpec([]byte(unknownField)); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("unknown field error = %v, want invalid task spec", err)
	}
	if _, err := DecodeTaskSpec([]byte(strings.Repeat("x", MaxTaskSpecBytes+1))); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("oversized document error = %v, want invalid task spec", err)
	}
	if _, err := DecodeTaskSpec(append(encoded, []byte(` {}`)...)); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("trailing JSON error = %v, want invalid task spec", err)
	}
}

func TestValidatePairDistinguishesSameSubjectControlsFromDifferentials(t *testing.T) {
	base := validTaskSpec()
	candidate := validTaskSpec()
	candidate.TaskSpecID = "spec-candidate"
	candidate.SubjectRole = "candidate"
	if err := ValidatePair(base, candidate); err != nil {
		t.Fatalf("same-subject control pair: %v", err)
	}

	base.ComparisonMode = "differential"
	candidate.ComparisonMode = "differential"
	candidate.Subject.Source.Identity.Digest = candidateDigest
	candidate.Subject.Identity.Digest = candidateDigest
	if err := ValidatePair(base, candidate); err != nil {
		t.Fatalf("distinct-subject differential pair: %v", err)
	}

	candidate.Subject.Identity.Digest = baseDigest
	candidate.Subject.Source.Identity.Digest = baseDigest
	if err := ValidatePair(base, candidate); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("same-subject differential error = %v, want invalid pair", err)
	}
}

func TestValidatePairRejectsCrossProfileAndGenerationBindings(t *testing.T) {
	base := validTaskSpec()
	candidate := validTaskSpec()
	candidate.TaskSpecID = "spec-candidate"
	candidate.SubjectRole = "candidate"
	candidate.Profile.Artifact.Digest = candidateDigest
	candidate.ProfileApproval.ApprovedProfileDigest = candidateDigest
	if err := ValidatePair(base, candidate); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("profile mismatch error = %v, want invalid pair", err)
	}

	candidate = validTaskSpec()
	candidate.TaskSpecID = "spec-candidate"
	candidate.SubjectRole = "candidate"
	candidate.Generation.Patch.Digest = candidateDigest
	if err := ValidatePair(base, candidate); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("generation mismatch error = %v, want invalid pair", err)
	}
}

func TestValidatePairRequiresMatchingExpectedRunRevisionAndDistinctTaskIDs(t *testing.T) {
	base := validTaskSpec()
	candidate := validTaskSpec()
	candidate.TaskSpecID = "spec-candidate"
	candidate.SubjectRole = "candidate"
	candidate.ExpectedRunRevision++
	if err := ValidatePair(base, candidate); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("revision mismatch error = %v, want invalid pair", err)
	}

	candidate.ExpectedRunRevision = base.ExpectedRunRevision
	candidate.TaskSpecID = base.TaskSpecID
	if err := ValidatePair(base, candidate); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("duplicate task spec ID error = %v, want invalid pair", err)
	}
}

func TestTaskSpecRejectsApprovalAndRetryPolicyDrift(t *testing.T) {
	spec := validTaskSpec()
	spec.ProfileApproval.ApprovedProfileDigest = candidateDigest
	if err := spec.Validate(); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("mismatched profile approval error = %v, want invalid spec", err)
	}

	spec = validTaskSpec()
	spec.RetryPolicy.RetryableFailures = []string{"test_assertion_failed"}
	if err := spec.Validate(); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("assertion retry error = %v, want invalid spec", err)
	}

	spec = validTaskSpec()
	spec.Generation.Tests.MediaType = "application/java-archive"
	if err := spec.Validate(); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("incorrect materialized-test media type error = %v, want invalid spec", err)
	}

	spec = validTaskSpec()
	spec.Subject.Source.Locator = "https://user:secret@example.invalid/repository.git"
	if err := spec.Validate(); !errors.Is(err, ErrInvalidTaskSpec) {
		t.Fatalf("credential-bearing source URL error = %v, want invalid spec", err)
	}
}

func TestTaskSpecificationDigestUsesExactStoredBytes(t *testing.T) {
	left := []byte(`{"task_spec_id":"spec-1"}`)
	right := []byte(`{ "task_spec_id": "spec-1" }`)
	if DigestTaskSpec(left) == DigestTaskSpec(right) {
		t.Fatal("different stored bytes unexpectedly share a specification digest")
	}
	if got := DigestTaskSpec(left); !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Fatalf("task specification digest = %q, want SHA-256", got)
	}
}

func validTaskSpec() TaskSpec {
	reviewer := Reviewer{
		Identity:         Identity{Kind: "person", ID: "reviewer-1"},
		Role:             "execution-reviewer",
		AuthorizationRef: "authz-review-1",
	}
	return TaskSpec{
		Contract:            "atg.execution_task_spec",
		Version:             "1.0",
		TaskSpecID:          "spec-base",
		RunID:               "run-1",
		ExpectedRunRevision: 1,
		PairID:              "pair-1",
		GenerationID:        "generation-1",
		SubjectRole:         "base",
		ComparisonMode:      "same_subject_control",
		Subject: Subject{
			Source: Source{
				Identity: Identity{Kind: "git-commit", ID: "commit-1", Digest: baseDigest},
				Locator:  "https://example.invalid/repository.git",
			},
			Identity: Identity{Kind: "source-tree", ID: "tree-1", Digest: baseDigest},
		},
		Generation: GenerationRefs{
			Manifest: Artifact{ArtifactID: "generation-manifest", MediaType: "application/vnd.atg.generation+json", Digest: manifestDigest},
			Patch:    Artifact{ArtifactID: "patch", MediaType: "text/x-diff", Digest: patchDigest},
			Tests:    Artifact{ArtifactID: "tests", MediaType: "application/vnd.atg.materialized-tests+json", Digest: testsDigest},
		},
		CodeApproval: CodeApproval{
			CheckpointID:             "code-checkpoint-1",
			DecisionID:               "code-decision-1",
			Reviewer:                 reviewer,
			ReviewedGenerationDigest: manifestDigest,
		},
		Profile: ProfileRef{
			ProfileID:      "profile-1",
			ProfileVersion: "1.0.0",
			Artifact:       Artifact{ArtifactID: "profile", MediaType: "application/vnd.atg.execution-profile+json", Digest: profileDigest},
		},
		ProfileApproval: ProfileApproval{
			DecisionID:            "profile-decision-1",
			Reviewer:              reviewer,
			ApprovedProfileDigest: profileDigest,
		},
		ExpectedTestIDs: []string{"fixture.SmokeTest#works"},
		RetryPolicy: RetryPolicy{
			PolicyID:              "retry-1",
			MaxAttempts:           2,
			DeadlineAt:            "2026-11-18T19:00:00Z",
			InitialBackoffSeconds: 10,
			MaxBackoffSeconds:     60,
			RetryableFailures:     []string{"worker_lost"},
		},
		CreatedAt: "2026-11-17T19:00:00Z",
	}
}
