package testgensim

import (
	"context"
	"regexp"
	"testing"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

func TestRunRetryScenario(t *testing.T) {
	report, err := Run(context.Background(), ScenarioRetry)
	if err != nil {
		t.Fatal(err)
	}
	if !report.SimulationOnly || report.Profile.Mode != "simulation-only" || report.Profile.Description == "" || report.Profile.ExternalEffects {
		t.Fatalf("profile is not clearly offline: %#v", report)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(report.Profile.Digest) || report.Run.Request.ProfileDigest != report.Profile.Digest {
		t.Fatalf("profile identity is not digest-bound: %#v", report.Profile)
	}
	if report.Run.Status != testgenflow.StatusCompleted || report.Run.AttemptCount() != 2 || report.Run.TestsAdded != 1 {
		t.Fatalf("retry scenario run = %#v", report.Run)
	}
	if report.Run.Attempts[0].Status != testgenflow.AttemptFailed || report.Run.Attempts[1].Status != testgenflow.AttemptAccepted {
		t.Fatalf("retry attempt outcomes = %#v", report.Run.Attempts)
	}
	if report.Run.Attempts[1].LastError != "" || report.GenerationTaskResults.Attempts != "2" {
		t.Fatalf("retry result projection = %#v", report.GenerationTaskResults)
	}
	if report.Run.PRURL != "https://example.invalid/simulated-change/1" {
		t.Fatalf("simulated PR result = %q", report.Run.PRURL)
	}
}

func TestRunSuccessScenarioCompletesInOneAttempt(t *testing.T) {
	report, err := Run(context.Background(), ScenarioSuccess)
	if err != nil {
		t.Fatal(err)
	}
	if report.Run.Status != testgenflow.StatusCompleted || report.Run.AttemptCount() != 1 || report.Run.TestsAdded != 1 {
		t.Fatalf("success scenario run = %#v", report.Run)
	}
	want := []string{
		"source:resolve",
		"maven:package",
		"generation:attempt-1",
		"maven:focused_tests",
		"maven:verify",
		"scm:simulate-pr",
	}
	if len(report.WorkerCalls) != len(want) {
		t.Fatalf("worker calls = %v, want %v", report.WorkerCalls, want)
	}
	for i := range want {
		if report.WorkerCalls[i] != want[i] {
			t.Fatalf("worker calls = %v, want %v", report.WorkerCalls, want)
		}
	}
}

func TestRunNoChangesScenarioSkipsFocusedAndFinalMavenPhases(t *testing.T) {
	report, err := Run(context.Background(), ScenarioNoChanges)
	if err != nil {
		t.Fatal(err)
	}
	if report.Run.Status != testgenflow.StatusCompleted || report.Run.TestsAdded != 0 || report.Run.PRURL != "" {
		t.Fatalf("no-change run = %#v", report.Run)
	}
	want := []string{"source:resolve", "maven:package", "generation:attempt-1"}
	if len(report.WorkerCalls) != len(want) {
		t.Fatalf("worker calls = %v, want %v", report.WorkerCalls, want)
	}
	for i := range want {
		if report.WorkerCalls[i] != want[i] {
			t.Fatalf("worker calls = %v, want %v", report.WorkerCalls, want)
		}
	}
}

func TestRunUnknownOutcomeDoesNotRetry(t *testing.T) {
	report, err := Run(context.Background(), ScenarioUnknownResult)
	if err != nil {
		t.Fatal(err)
	}
	if report.Run.Status != testgenflow.StatusOutcomeUnknown || report.Run.AttemptCount() != 0 {
		t.Fatalf("unknown outcome run = %#v", report.Run)
	}
	if len(report.WorkerCalls) != 1 || report.WorkerCalls[0] != "source:resolve" {
		t.Fatalf("unknown outcome retried work: %v", report.WorkerCalls)
	}
}

func TestRunRejectsUnknownScenario(t *testing.T) {
	if _, err := Run(context.Background(), Scenario("execute-arbitrary-command")); err == nil {
		t.Fatal("unknown simulation scenario was accepted")
	}
}
