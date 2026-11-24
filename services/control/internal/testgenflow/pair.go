package testgenflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// SubjectOutcome records one immutable subject's execution outcome.
type SubjectOutcome struct {
	Subject     string `json:"subject"`
	Commit      string `json:"commit"`
	Succeeded   bool   `json:"succeeded"`
	EvidenceRef string `json:"evidence_ref"`
	Diagnostic  string `json:"diagnostic,omitempty"`
}

// PairEvidence is the provider-neutral differential phase document: the same
// admitted test artifact executed against both pinned subjects, with
// per-subject outcomes. Execution providers return this shape so oracle
// enforcement does not depend on a specific worker composition.
type PairEvidence struct {
	SchemaVersion        string           `json:"schema_version"`
	Phase                string           `json:"phase"`
	RunID                string           `json:"run_id"`
	ProfileDigest        string           `json:"profile_digest"`
	TestArtifactDigest   string           `json:"test_artifact_digest,omitempty"`
	PatchDigest          string           `json:"patch_digest,omitempty"`
	Subjects             []SubjectOutcome `json:"subjects"`
	Succeeded            bool             `json:"succeeded"`
	DifferentialObserved bool             `json:"differential_observed"`
	Diagnostic           string           `json:"diagnostic,omitempty"`
}

// ArtifactReader reads retained artifacts by digest. It is the minimal
// provider-neutral dependency for oracle enforcement.
type ArtifactReader interface {
	Get(ctx context.Context, ref string, maxBytes int64) ([]byte, error)
}

const maxOracleArtifactBytes = 32 << 20

// DifferentialOracleMismatch compares a successful focused differential pair
// against the approved plan's declared expected outcomes. A pair that did not
// discriminate (a subject declared to fail actually passed) is a failed
// observation, not a pass, so it enters the repair loop instead of
// terminating the run. Absent or unreadable declared outcomes disable the
// check; a mismatch can only fail an attempt, never pass one.
func DifferentialOracleMismatch(ctx context.Context, artifacts ArtifactReader, run Run, work MavenWorkRequest, result MavenResult) (string, bool) {
	if artifacts == nil {
		return "", false
	}
	paths := work.ChangedTestPaths
	if len(paths) == 0 {
		paths = run.ChangedTestPaths
	}
	if len(paths) == 0 {
		return "", false
	}
	raw, err := artifacts.Get(ctx, result.EvidenceRef, maxOracleArtifactBytes)
	if err != nil {
		return "", false
	}
	var pair PairEvidence
	artifactDigest := work.TestArtifactDigest
	if artifactDigest == "" && run.Candidate != nil {
		artifactDigest = run.Candidate.TestArtifactDigest
	}
	if json.Unmarshal(raw, &pair) != nil || len(pair.Subjects) != 2 || pair.ProfileDigest != run.Request.ProfileDigest || pair.TestArtifactDigest != artifactDigest {
		return "", false
	}
	declared, err := declaredOutcomes(ctx, artifacts, run, work.Attempt, paths)
	if err != nil || len(declared) == 0 {
		return "", false
	}
	var mismatches []string
	for _, subject := range pair.Subjects {
		if declared[subject.Subject] == "fail" && subject.Succeeded {
			mismatches = append(mismatches, fmt.Sprintf("subject %s was declared to fail but passed", subject.Subject))
		}
	}
	if len(mismatches) == 0 {
		return "", false
	}
	return "differential did not match the approved plan oracle: " + strings.Join(mismatches, "; ") + ". The test input does not discriminate between the pinned subjects; revise the input mechanism until the declared base/candidate outcomes are observed", true
}

// declaredOutcomes reads the approved plan's declared expected_outcomes for
// the scenarios the admitted candidate implemented. Plan output is untrusted
// data: it was human-approved, and here it can only require an observation to
// fail.
func declaredOutcomes(ctx context.Context, artifacts ArtifactReader, run Run, attempt int, paths []string) (map[string]string, error) {
	planRef := ""
	for i := len(run.Reviews) - 1; i >= 0; i-- {
		review := run.Reviews[i]
		if review.Stage == "plan" && review.Action == "approve" && review.Attempt == attempt {
			planRef = review.ArtifactRef
			break
		}
	}
	if planRef == "" {
		return nil, nil
	}
	stageRaw, err := artifacts.Get(ctx, planRef, maxOracleArtifactBytes)
	if err != nil {
		return nil, err
	}
	var stage struct {
		ResultRef string `json:"result_ref"`
	}
	if json.Unmarshal(stageRaw, &stage) != nil || stage.ResultRef == "" {
		return nil, nil
	}
	planRaw, err := artifacts.Get(ctx, stage.ResultRef, maxOracleArtifactBytes)
	if err != nil {
		return nil, err
	}
	var plan struct {
		Output struct {
			Scenarios []struct {
				TestPath         string            `json:"test_path"`
				ExpectedOutcomes map[string]string `json:"expected_outcomes"`
			} `json:"scenarios"`
		} `json:"output"`
	}
	if json.Unmarshal(planRaw, &plan) != nil {
		return nil, nil
	}
	declared := map[string]string{}
	for _, scenario := range plan.Output.Scenarios {
		if !slices.Contains(paths, scenario.TestPath) {
			continue
		}
		for subject, expectation := range scenario.ExpectedOutcomes {
			if expectation == "fail" {
				declared[subject] = "fail"
			}
		}
	}
	return declared, nil
}
