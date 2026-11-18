package postgres

import (
	"testing"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
)

func TestAttemptEvidenceEligibilityRetainsOriginalOutcome(t *testing.T) {
	for _, test := range []struct {
		state   string
		outcome execution.ResultStatus
		want    bool
	}{
		{"completed", execution.ResultCompleted, true},
		{"completed", execution.ResultFailed, true},
		{"failed", execution.ResultFailed, true},
		{"failed", execution.ResultOutcomeUnknown, true},
		{"failed", execution.ResultCompleted, false},
		{"cancelled", execution.ResultCancelled, true},
		{"cancelled", execution.ResultCompleted, false},
		{"outcome_unknown", execution.ResultOutcomeUnknown, true},
		{"outcome_unknown", execution.ResultCompleted, false},
		{"running", execution.ResultCompleted, false},
		{"prepared", execution.ResultFailed, false},
	} {
		if got := eligibleResultState(resultAttempt{status: test.state}, execution.ExecutionResult{ObservedOutcome: test.outcome}); got != test.want {
			t.Errorf("attempt=%s outcome=%s eligibility=%t want=%t", test.state, test.outcome, got, test.want)
		}
	}
}

func TestHistoricalEvidenceCannotProjectOntoCurrentTask(t *testing.T) {
	result := execution.ExecutionResult{AttemptID: "old-attempt"}
	for _, status := range []string{"collecting", "failed", "cancel_requested", "cancelled", "outcome_unknown", "completed", "superseded"} {
		if resultAppliesToTask(execution.TaskRecord{Status: status, ActiveAttemptID: "new-attempt"}, result) {
			t.Errorf("historical evidence can alter active %s task", status)
		}
		if resultAppliesToTask(execution.TaskRecord{Status: status}, result) {
			t.Errorf("historical evidence can alter inactive %s task", status)
		}
	}
	for _, status := range []string{"collecting", "failed", "cancel_requested", "cancelled", "outcome_unknown"} {
		if !resultAppliesToTask(execution.TaskRecord{Status: status, ActiveAttemptID: result.AttemptID}, result) {
			t.Errorf("current evidence cannot attach to %s task", status)
		}
	}
}
