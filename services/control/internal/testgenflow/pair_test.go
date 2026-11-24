package testgenflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
)

func putPairArtifact(t *testing.T, store *artifact.FileStore, data []byte) string {
	t.Helper()
	ref, err := store.Put(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	return ref.Digest
}

func TestDifferentialOracleMismatchFailsNonDiscriminatingPair(t *testing.T) {
	store, err := artifact.NewFileStore(t.TempDir(), maxOracleArtifactBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	profileDigest := "sha256:" + strings.Repeat("a", 64)
	testPath := "src/test/java/example/GeneratedTest.java"
	artifactDigest := digest([]byte("approved tests"))
	planOut := fmt.Sprintf(`{"output":{"scenarios":[{"test_path":%q,"expected_outcomes":{"base":"fail","candidate":"pass"}}]}}`, testPath)
	planRef := putPairArtifact(t, store, []byte(planOut))
	stageRef := putPairArtifact(t, store, []byte(`{"result_ref":"`+planRef+`"}`))
	pair := PairEvidence{SchemaVersion: "1", Phase: "focused_tests", RunID: "run-1", ProfileDigest: profileDigest, TestArtifactDigest: artifactDigest, Subjects: []SubjectOutcome{
		{Subject: "base", Commit: strings.Repeat("c", 40), Succeeded: true, EvidenceRef: "sha256:" + strings.Repeat("e", 64)},
		{Subject: "candidate", Commit: strings.Repeat("d", 40), Succeeded: true, EvidenceRef: "sha256:" + strings.Repeat("f", 64)},
	}, Succeeded: true}
	pairRef := putPairArtifact(t, store, mustPairJSON(t, pair))
	run := Run{RunID: "run-1", Request: Request{ProfileDigest: profileDigest, InputMode: InputModeModelGeneration}, ChangedTestPaths: []string{testPath}, Reviews: []GenerationReview{{Stage: "plan", Action: "approve", Attempt: 1, ArtifactRef: stageRef}}}
	work := MavenWorkRequest{WorkRequest: WorkRequest{RunID: "run-1", Attempt: 1, TestArtifactDigest: artifactDigest}, Phase: MavenPhaseFocusedTests}
	result := MavenResult{Succeeded: true, EvidenceRef: pairRef}

	diagnostic, mismatch := DifferentialOracleMismatch(context.Background(), store, run, work, result)
	if !mismatch || !strings.Contains(diagnostic, "subject base was declared to fail but passed") || !strings.Contains(diagnostic, "does not discriminate") {
		t.Fatalf("non-discriminating pair not failed: mismatch=%t diagnostic=%q", mismatch, diagnostic)
	}

	pair.Subjects[0].Succeeded = false
	discriminating := MavenResult{Succeeded: true, EvidenceRef: putPairArtifact(t, store, mustPairJSON(t, pair))}
	if _, mismatch = DifferentialOracleMismatch(context.Background(), store, run, work, discriminating); mismatch {
		t.Fatal("discriminating pair was failed against its declared oracle")
	}

	noReviews := run
	noReviews.Reviews = nil
	if _, mismatch = DifferentialOracleMismatch(context.Background(), store, noReviews, work, result); mismatch {
		t.Fatal("pair without a declared oracle was failed")
	}
	if _, mismatch = DifferentialOracleMismatch(context.Background(), nil, run, work, result); mismatch {
		t.Fatal("pair without artifact access was failed")
	}
}

func TestCoordinatorFailsNonDiscriminatingFocusedPairAgainstDeclaredOracle(t *testing.T) {
	store, err := artifact.NewFileStore(t.TempDir(), maxOracleArtifactBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	run := startGeneration(t, modelRun(t))
	testPath := "src/test/java/example/GeneratedTest.java"
	planOut := fmt.Sprintf(`{"output":{"scenarios":[{"test_path":%q,"expected_outcomes":{"base":"fail","candidate":"pass"}}]}}`, testPath)
	planRef := putPairArtifact(t, store, []byte(planOut))
	stageRef := putPairArtifact(t, store, []byte(`{"result_ref":"`+planRef+`"}`))
	for _, stage := range []string{"plan", "code"} {
		checkpoint := &GenerationCheckpoint{ID: GenerationCheckpointID(run.RunID, run.RequestDigest, 1, stage, stageRef), Stage: stage, ArtifactRef: stageRef, Attempt: 1, ExpiresAt: run.UpdatedAt.Add(time.Hour)}
		run = apply(t, run, Event{Type: EventGenerationCheckpointRequested, Checkpoint: checkpoint}, run.UpdatedAt.Add(time.Second))
		review := modelReview(run, "approve")
		run = apply(t, run, Event{Type: EventGenerationReviewDecided, Review: &review}, review.DecidedAt)
	}
	run = completeModelGeneration(t, run)
	pair := PairEvidence{SchemaVersion: "1", Phase: "focused_tests", RunID: run.RunID, ProfileDigest: run.Request.ProfileDigest, TestArtifactDigest: run.Attempts[len(run.Attempts)-1].TestArtifactDigest, Subjects: []SubjectOutcome{
		{Subject: "base", Commit: strings.Repeat("c", 40), Succeeded: true, EvidenceRef: "sha256:" + strings.Repeat("e", 64)},
		{Subject: "candidate", Commit: strings.Repeat("d", 40), Succeeded: true, EvidenceRef: "sha256:" + strings.Repeat("f", 64)},
	}, Succeeded: true}
	pairRef := putPairArtifact(t, store, mustPairJSON(t, pair))

	repository := newMemoryRepository()
	if _, _, err := repository.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	workers := &fakeTestGenerationWorkers{maven: []MavenResult{{Succeeded: true, EvidenceRef: pairRef}}}
	coordinator := Coordinator{Repository: repository, Generation: workers, Maven: workers, Artifacts: store, Now: func() time.Time { return run.UpdatedAt }}
	updated, err := coordinator.Advance(context.Background(), run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Stage != StageGenerate || updated.Attempts[len(updated.Attempts)-1].Status != AttemptFailed || !strings.Contains(updated.LastError, "declared to fail but passed") {
		t.Fatalf("non-discriminating pair not routed to repair: stage=%s lastError=%q", updated.Stage, updated.LastError)
	}

	unwired := newMemoryRepository()
	recreated := cloneRun(run)
	if _, _, err := unwired.CreateRun(context.Background(), recreated); err != nil {
		t.Fatal(err)
	}
	workers = &fakeTestGenerationWorkers{maven: []MavenResult{{Succeeded: true, EvidenceRef: pairRef}}}
	coordinator = Coordinator{Repository: unwired, Generation: workers, Maven: workers, Now: func() time.Time { return recreated.UpdatedAt }}
	updated, err = coordinator.Advance(context.Background(), recreated.RunID)
	if err != nil || updated.Stage != StageFinalVerify || updated.Attempts[len(updated.Attempts)-1].Status != AttemptAccepted {
		t.Fatalf("coordinator without artifact access must not enforce the oracle: stage=%s err=%v", updated.Stage, err)
	}
}

func mustPairJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
