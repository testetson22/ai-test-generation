package execution

import (
	"fmt"
	"slices"
	"time"
)

const ExecutionResultMediaType = "application/vnd.atg.execution-result+json"

const (
	MaxResultTestIDs     = 10_000
	MaxResultTestIDBytes = 1_024
	MaxTotalTestIDBytes  = 500_000
)

type ResultStatus string

const (
	ResultProvisional    ResultStatus = "provisional"
	ResultCompleted      ResultStatus = "completed"
	ResultFailed         ResultStatus = "failed"
	ResultBlocked        ResultStatus = "blocked"
	ResultNotReached     ResultStatus = "not_reached"
	ResultCancelled      ResultStatus = "cancelled"
	ResultOutcomeUnknown ResultStatus = "outcome_unknown"
)

type ExecutionResult struct {
	Contract             string                `json:"contract"`
	Version              string                `json:"version"`
	ResultID             string                `json:"result_id"`
	TaskID               string                `json:"task_id"`
	RunID                string                `json:"run_id"`
	PairID               string                `json:"pair_id"`
	AttemptID            string                `json:"attempt_id"`
	AttemptNumber        int                   `json:"attempt_number"`
	ExpectedTaskRevision int64                 `json:"expected_task_revision"`
	TaskSpecDigest       string                `json:"task_spec_digest"`
	SubjectRole          string                `json:"subject_role"`
	Source               Identity              `json:"source"`
	Subject              Identity              `json:"subject"`
	Profile              ResultProfile         `json:"profile"`
	Provider             Identity              `json:"provider"`
	ProviderJob          Identity              `json:"provider_job"`
	Worker               Identity              `json:"worker"`
	Environment          Identity              `json:"environment"`
	Status               ResultStatus          `json:"status"`
	ObservedOutcome      ResultStatus          `json:"observed_outcome"`
	FailureCategory      string                `json:"failure_category,omitempty"`
	Phases               []ExecutionPhase      `json:"phases"`
	Tests                TestExecutionEvidence `json:"tests"`
	Artifacts            []Artifact            `json:"artifacts"`
	Cleanup              CleanupEvidence       `json:"cleanup"`
	StartedAt            string                `json:"started_at"`
	UpdatedAt            string                `json:"updated_at"`
	FinalizedAt          string                `json:"finalized_at,omitempty"`
}

type ResultProfile struct {
	ProfileID      string `json:"profile_id"`
	ProfileVersion string `json:"profile_version"`
	Digest         string `json:"digest"`
}

type ExecutionPhase struct {
	Phase           string     `json:"phase"`
	CommandID       string     `json:"command_id"`
	Status          string     `json:"status"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	FailureCategory string     `json:"failure_category,omitempty"`
	StartedAt       string     `json:"started_at"`
	FinishedAt      string     `json:"finished_at"`
	Artifacts       []Artifact `json:"artifacts,omitempty"`
}

type TestExecutionEvidence struct {
	ExpectedTestIDs   []string   `json:"expected_test_ids"`
	DiscoveredTestIDs []string   `json:"discovered_test_ids"`
	SelectedTestIDs   []string   `json:"selected_test_ids"`
	ExecutedTestIDs   []string   `json:"executed_test_ids"`
	SkippedTestIDs    []string   `json:"skipped_test_ids"`
	ReportArtifacts   []Artifact `json:"report_artifacts,omitempty"`
}

type CleanupEvidence struct {
	Status               string `json:"status"`
	TerminationConfirmed bool   `json:"termination_confirmed"`
	FailureCategory      string `json:"failure_category,omitempty"`
	FinishedAt           string `json:"finished_at,omitempty"`
}

func (r ExecutionResult) Validate() error {
	if r.Contract != "atg.execution_result" || r.Version != "1.0" {
		return fmt.Errorf("unsupported execution result contract or version")
	}
	for name, id := range map[string]string{
		"result_id": r.ResultID, "task_id": r.TaskID, "run_id": r.RunID, "pair_id": r.PairID, "attempt_id": r.AttemptID,
	} {
		if !validID(id) {
			return fmt.Errorf("execution result %s is invalid", name)
		}
	}
	if r.AttemptNumber < 1 || r.AttemptNumber > 10 || r.ExpectedTaskRevision < 1 || !validDigest(r.TaskSpecDigest) {
		return fmt.Errorf("execution result attempt, revision, or task specification digest is invalid")
	}
	if r.SubjectRole != "base" && r.SubjectRole != "candidate" {
		return fmt.Errorf("execution result subject role is invalid")
	}
	if err := validateIdentity(r.Source, true); err != nil {
		return fmt.Errorf("execution result source identity: %w", err)
	}
	if err := validateIdentity(r.Subject, true); err != nil {
		return fmt.Errorf("execution result subject identity: %w", err)
	}
	if !validID(r.Profile.ProfileID) || !semverPattern.MatchString(r.Profile.ProfileVersion) || !validDigest(r.Profile.Digest) {
		return fmt.Errorf("execution result profile identity is incomplete")
	}
	for name, identity := range map[string]Identity{
		"provider": r.Provider, "provider job": r.ProviderJob,
	} {
		if err := validateIdentity(identity, false); err != nil {
			return fmt.Errorf("execution result %s identity: %w", name, err)
		}
	}
	for name, identity := range map[string]Identity{"worker": r.Worker, "environment": r.Environment} {
		if err := validateIdentity(identity, true); err != nil {
			return fmt.Errorf("execution result %s identity: %w", name, err)
		}
	}
	if !validResultStatus(r.Status) || !validFinalResultStatus(r.ObservedOutcome) {
		return fmt.Errorf("execution result status is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.StartedAt); err != nil {
		return fmt.Errorf("execution result start time is invalid")
	}
	updated, err := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("execution result update time is invalid")
	}
	started, _ := time.Parse(time.RFC3339Nano, r.StartedAt)
	if updated.Before(started) {
		return fmt.Errorf("execution result update precedes start")
	}
	if r.Status == ResultProvisional {
		if r.Cleanup.Status != "pending" || r.Cleanup.TerminationConfirmed || r.FinalizedAt != "" {
			return fmt.Errorf("provisional result requires pending cleanup and no final timestamp")
		}
	} else {
		if _, err := time.Parse(time.RFC3339Nano, r.FinalizedAt); err != nil {
			return fmt.Errorf("finalized result requires a valid final timestamp")
		}
		if r.UpdatedAt != r.FinalizedAt {
			return fmt.Errorf("finalized result update and final timestamps must match")
		}
	}
	if r.Status == ResultCompleted && (r.ObservedOutcome != ResultCompleted || !r.Cleanup.TerminationConfirmed || r.Cleanup.Status != "succeeded" && r.Cleanup.Status != "not_required") {
		return fmt.Errorf("completed execution result requires completed observation and confirmed cleanup")
	}
	if r.Status == ResultFailed && !validFailureCategory(r.FailureCategory) {
		return fmt.Errorf("failed execution result requires a failure category")
	}
	if r.ObservedOutcome == ResultFailed && !validFailureCategory(r.FailureCategory) {
		return fmt.Errorf("failed observed outcome requires a failure category")
	}
	if !slices.Contains([]string{"pending", "succeeded", "failed", "not_required", "unknown"}, r.Cleanup.Status) {
		return fmt.Errorf("execution result cleanup status is invalid")
	}
	if r.Cleanup.Status == "failed" && r.Cleanup.FailureCategory != "cleanup_failed" {
		return fmt.Errorf("failed cleanup requires cleanup_failed category")
	}
	if r.Cleanup.Status == "succeeded" || r.Cleanup.Status == "not_required" {
		if !r.Cleanup.TerminationConfirmed {
			return fmt.Errorf("successful or unnecessary cleanup requires confirmed termination")
		}
	}
	if r.Cleanup.Status == "not_required" && r.ObservedOutcome != ResultNotReached {
		return fmt.Errorf("cleanup is unnecessary only when execution was not reached")
	}
	if r.Cleanup.FinishedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, r.Cleanup.FinishedAt); err != nil {
			return fmt.Errorf("execution result cleanup time is invalid")
		}
	}
	if len(r.Phases) == 0 || len(r.Phases) > 32 {
		return fmt.Errorf("execution result must contain 1..32 phase observations")
	}
	for _, phase := range r.Phases {
		if !validID(phase.CommandID) || !slices.Contains([]string{"prepare", "build", "discover", "test", "collect", "application_start", "application_reset", "cleanup"}, phase.Phase) || !slices.Contains([]string{"succeeded", "failed", "skipped", "not_reached", "unknown"}, phase.Status) {
			return fmt.Errorf("execution result phase identity or status is invalid")
		}
		phaseStart, startErr := time.Parse(time.RFC3339Nano, phase.StartedAt)
		phaseFinish, finishErr := time.Parse(time.RFC3339Nano, phase.FinishedAt)
		if startErr != nil || finishErr != nil || phaseFinish.Before(phaseStart) {
			return fmt.Errorf("execution result phase timestamps are invalid")
		}
		if phase.Status == "failed" && !validFailureCategory(phase.FailureCategory) {
			return fmt.Errorf("failed execution phase requires a failure category")
		}
	}
	testIDBytes := 0
	for _, values := range [][]string{r.Tests.ExpectedTestIDs, r.Tests.DiscoveredTestIDs, r.Tests.SelectedTestIDs, r.Tests.ExecutedTestIDs, r.Tests.SkippedTestIDs} {
		for _, value := range values {
			testIDBytes += len(value)
		}
	}
	if r.Artifacts == nil || r.Tests.DiscoveredTestIDs == nil || r.Tests.SelectedTestIDs == nil || r.Tests.ExecutedTestIDs == nil || r.Tests.SkippedTestIDs == nil || testIDBytes > MaxTotalTestIDBytes || len(r.Tests.ExpectedTestIDs) == 0 || !uniqueNonEmpty(r.Tests.ExpectedTestIDs) || !uniqueNonEmpty(r.Tests.DiscoveredTestIDs) || !uniqueNonEmpty(r.Tests.SelectedTestIDs) || !uniqueNonEmpty(r.Tests.ExecutedTestIDs) || !uniqueNonEmpty(r.Tests.SkippedTestIDs) {
		return fmt.Errorf("execution result test identities are invalid")
	}
	expected := make(map[string]struct{}, len(r.Tests.ExpectedTestIDs))
	for _, testID := range r.Tests.ExpectedTestIDs {
		expected[testID] = struct{}{}
	}
	selected := make(map[string]struct{}, len(r.Tests.SelectedTestIDs))
	for _, testID := range r.Tests.SelectedTestIDs {
		if _, ok := expected[testID]; !ok {
			return fmt.Errorf("selected test is not in the approved test set")
		}
		selected[testID] = struct{}{}
	}
	for _, testID := range append(append([]string(nil), r.Tests.ExecutedTestIDs...), r.Tests.SkippedTestIDs...) {
		if _, ok := selected[testID]; !ok {
			return fmt.Errorf("executed or skipped test is not selected")
		}
	}
	for _, testID := range r.Tests.ExecutedTestIDs {
		if slices.Contains(r.Tests.SkippedTestIDs, testID) {
			return fmt.Errorf("test cannot be both executed and skipped")
		}
	}
	allArtifacts := append(append([]Artifact(nil), r.Artifacts...), r.Tests.ReportArtifacts...)
	for _, phase := range r.Phases {
		allArtifacts = append(allArtifacts, phase.Artifacts...)
	}
	if len(allArtifacts) > 100 {
		return fmt.Errorf("execution result artifact count exceeds limit")
	}
	for _, artifact := range allArtifacts {
		if err := validateArtifact(artifact); err != nil {
			return fmt.Errorf("execution result artifact: %w", err)
		}
	}
	return nil
}

func validResultStatus(status ResultStatus) bool {
	return status == ResultProvisional || validFinalResultStatus(status)
}

func validFinalResultStatus(status ResultStatus) bool {
	return status == ResultCompleted || status == ResultFailed || status == ResultBlocked || status == ResultNotReached || status == ResultCancelled || status == ResultOutcomeUnknown
}

func validFailureCategory(category string) bool {
	return slices.Contains([]string{
		"invalid_input", "unauthorized", "profile_unapproved", "capability_unavailable", "transport_failed",
		"provider_unavailable", "submission_uncertain", "worker_setup_failed", "source_mismatch",
		"materialization_failed", "build_failed", "test_collection_failed", "test_assertion_failed",
		"report_invalid", "timed_out", "worker_resource_exhausted", "worker_lost", "artifact_transport_failed",
		"cleanup_failed", "unknown",
	}, category)
}

func uniqueNonEmpty(values []string) bool {
	if len(values) > MaxResultTestIDs {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || len(value) > MaxResultTestIDBytes {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}
