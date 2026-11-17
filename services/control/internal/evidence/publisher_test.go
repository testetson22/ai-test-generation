package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
)

func TestExecutionResultFailedTestsMustBeExecuted(t *testing.T) {
	result, _ := provisionalResult()
	if err := result.Validate(); err != nil {
		t.Fatalf("valid provisional result: %v", err)
	}
	result.Tests.FailedTestIDs = []string{"example.OtherTest#not-run"}
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), "failed test is not in the executed test set") {
		t.Fatalf("failed test outside execution inventory error = %v", err)
	}
}

func TestPublisherPersistsEvidenceBeforeProvisionalDeliveryAndFinalizesAfterCleanup(t *testing.T) {
	store, err := artifact.NewFileStore(t.TempDir(), 1_000_000)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	defer store.Close()
	recorder := &recordingResultStore{store: store}
	publisher, err := NewPublisher(store, recorder, 10_000)
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}
	result, content := provisionalResult()
	published, err := publisher.PublishProvisional(context.Background(), result, []Content{content})
	if err != nil {
		t.Fatalf("publish provisional evidence: %v", err)
	}
	if len(recorder.calls) != 1 || recorder.calls[0] != "provisional" || recorder.provisional.Status != execution.ResultProvisional {
		t.Fatalf("delivery calls = %v, result status = %q", recorder.calls, recorder.provisional.Status)
	}
	if stored, err := store.Get(context.Background(), content.Artifact.Digest, 10_000); err != nil || string(stored) != string(content.Bytes) {
		t.Fatalf("evidence availability at provisional delivery = %q, %v", stored, err)
	}

	finished := time.Date(2026, time.November, 17, 19, 2, 0, 0, time.UTC)
	finalRef, err := publisher.Finalize(context.Background(), published, "succeeded", true, finished)
	if err != nil {
		t.Fatalf("finalize after cleanup: %v", err)
	}
	if len(recorder.calls) != 2 || recorder.calls[1] != "finalized" || recorder.finalized.Status != execution.ResultCompleted {
		t.Fatalf("delivery calls = %v, final status = %q", recorder.calls, recorder.finalized.Status)
	}
	if finalRef.Digest == published.Reference.Digest || finalRef.ArtifactID != published.Reference.ArtifactID {
		t.Fatalf("final result reference = %#v, provisional = %#v", finalRef, published.Reference)
	}
	finalBytes, err := store.Get(context.Background(), finalRef.Digest, MaxResultBytes)
	if err != nil {
		t.Fatalf("read finalized result: %v", err)
	}
	var finalized execution.ExecutionResult
	if err := json.Unmarshal(finalBytes, &finalized); err != nil {
		t.Fatalf("decode finalized result: %v", err)
	}
	if finalized.Status != execution.ResultCompleted || !finalized.Cleanup.TerminationConfirmed || finalized.FinalizedAt != finished.Format(time.RFC3339Nano) {
		t.Fatalf("finalized execution evidence = %#v", finalized)
	}
}

func TestPublisherNeverRecordsResultWhenArtifactDurabilityFails(t *testing.T) {
	backing, err := artifact.NewFileStore(t.TempDir(), 1_000_000)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	defer backing.Close()
	store := failingStore{Store: backing, putErr: errors.New("storage unavailable")}
	recorder := &recordingResultStore{store: backing}
	publisher, err := NewPublisher(store, recorder, 10_000)
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}
	result, content := provisionalResult()
	if _, err := publisher.PublishProvisional(context.Background(), result, []Content{content}); err == nil {
		t.Fatal("publish succeeded while artifact storage was unavailable")
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("result was delivered despite failed persistence: %v", recorder.calls)
	}
}

func TestPublisherRecoversLostArtifactAcknowledgementByRetryingTheSameDigest(t *testing.T) {
	backing, err := artifact.NewFileStore(t.TempDir(), 1_000_000)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	defer backing.Close()
	store := &loseAcknowledgementOnce{Store: backing}
	recorder := &recordingResultStore{store: backing}
	publisher, err := NewPublisher(store, recorder, 10_000)
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}
	result, content := provisionalResult()
	if _, err := publisher.PublishProvisional(context.Background(), result, []Content{content}); err == nil {
		t.Fatal("lost artifact acknowledgement was accepted")
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("provisional result was recorded after lost acknowledgement: %v", recorder.calls)
	}
	if _, err := publisher.PublishProvisional(context.Background(), result, []Content{content}); err != nil {
		t.Fatalf("retry same immutable evidence digest: %v", err)
	}
	if len(recorder.calls) != 1 || recorder.calls[0] != "provisional" {
		t.Fatalf("retry delivery calls = %v, want one provisional record", recorder.calls)
	}
}

func TestPublisherRejectsMismatchedEvidenceAndCleanupCannotClaimSuccess(t *testing.T) {
	store, err := artifact.NewFileStore(t.TempDir(), 1_000_000)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	defer store.Close()
	recorder := &recordingResultStore{store: store}
	publisher, err := NewPublisher(store, recorder, 10_000)
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}
	result, content := provisionalResult()
	content.Bytes = []byte("tampered evidence")
	if _, err := publisher.PublishProvisional(context.Background(), result, []Content{content}); err == nil || !strings.Contains(err.Error(), "digest verification") {
		t.Fatalf("tampered artifact error = %v", err)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("tampered evidence was delivered: %v", recorder.calls)
	}

	result, content = provisionalResult()
	published, err := publisher.PublishProvisional(context.Background(), result, []Content{content})
	if err != nil {
		t.Fatalf("publish valid evidence: %v", err)
	}
	if _, err := publisher.Finalize(context.Background(), published, "succeeded", false, time.Now().UTC()); err == nil {
		t.Fatal("successful cleanup without confirmed termination was accepted")
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("invalid cleanup was finalized: %v", recorder.calls)
	}
}

func TestPublisherRetainsCleanupFailureAndUnknownTermination(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		cleanupStatus string
		termination   bool
		wantStatus    execution.ResultStatus
		wantFailure   string
	}{
		{name: "cleanup failed", cleanupStatus: "failed", wantStatus: execution.ResultFailed, wantFailure: "cleanup_failed"},
		{name: "termination unknown", cleanupStatus: "unknown", wantStatus: execution.ResultOutcomeUnknown},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, err := artifact.NewFileStore(t.TempDir(), 1_000_000)
			if err != nil {
				t.Fatalf("open artifact store: %v", err)
			}
			defer store.Close()
			recorder := &recordingResultStore{store: store}
			publisher, err := NewPublisher(store, recorder, 10_000)
			if err != nil {
				t.Fatalf("create publisher: %v", err)
			}
			result, content := provisionalResult()
			published, err := publisher.PublishProvisional(context.Background(), result, []Content{content})
			if err != nil {
				t.Fatalf("publish provisional evidence: %v", err)
			}
			finalRef, err := publisher.Finalize(context.Background(), published, testCase.cleanupStatus, testCase.termination, time.Now().UTC())
			if err != nil {
				t.Fatalf("finalize result: %v", err)
			}
			if recorder.finalized.Status != testCase.wantStatus || recorder.finalized.FailureCategory != testCase.wantFailure {
				t.Fatalf("final result status/category = %q/%q, want %q/%q", recorder.finalized.Status, recorder.finalized.FailureCategory, testCase.wantStatus, testCase.wantFailure)
			}
			if _, err := store.Get(context.Background(), finalRef.Digest, MaxResultBytes); err != nil {
				t.Fatalf("finalized evidence was not retained: %v", err)
			}
		})
	}
}

type recordingResultStore struct {
	store       artifact.Store
	calls       []string
	provisional execution.ExecutionResult
	finalized   execution.ExecutionResult
}

func (r *recordingResultStore) RecordProvisional(ctx context.Context, result execution.ExecutionResult, reference execution.Artifact) error {
	if result.Status != execution.ResultProvisional {
		return errors.New("provisional delivery received a finalized result")
	}
	if _, err := r.store.Get(ctx, reference.Digest, MaxResultBytes); err != nil {
		return err
	}
	r.calls = append(r.calls, "provisional")
	r.provisional = result
	return nil
}

func (r *recordingResultStore) RecordFinalized(ctx context.Context, result execution.ExecutionResult, provisional, finalized execution.Artifact) error {
	if result.Status == execution.ResultProvisional || provisional.Digest == finalized.Digest {
		return errors.New("final delivery is not distinct from provisional result")
	}
	if _, err := r.store.Get(ctx, finalized.Digest, MaxResultBytes); err != nil {
		return err
	}
	r.calls = append(r.calls, "finalized")
	r.finalized = result
	return nil
}

type failingStore struct {
	artifact.Store
	putErr error
}

func (s failingStore) Put(context.Context, []byte) (artifact.Ref, error) {
	return artifact.Ref{}, s.putErr
}

type loseAcknowledgementOnce struct {
	artifact.Store
	lost bool
}

func (s *loseAcknowledgementOnce) Put(ctx context.Context, content []byte) (artifact.Ref, error) {
	reference, err := s.Store.Put(ctx, content)
	if err != nil {
		return artifact.Ref{}, err
	}
	if !s.lost {
		s.lost = true
		return reference, errors.New("upload acknowledgement lost")
	}
	return reference, nil
}

func provisionalResult() (execution.ExecutionResult, Content) {
	logBytes := []byte("worker output")
	logReference := execution.Artifact{
		ArtifactID: "artifact-worker-log-1", MediaType: "text/plain", Digest: artifact.Digest(logBytes), SizeBytes: int64(len(logBytes)),
	}
	return execution.ExecutionResult{
		Contract: "atg.execution_result", Version: "1.0", ResultID: "result-task-1-attempt-1",
		TaskID: "task-1", RunID: "run-1", PairID: "pair-1", AttemptID: "attempt-1", AttemptNumber: 1,
		ExpectedTaskRevision: 4, TaskSpecDigest: "sha256:" + strings.Repeat("a", 64), SubjectRole: "base",
		Source: Identity("git-commit", "commit-1", "1"), Subject: Identity("git-tree", "subject-1", "2"),
		Profile:  execution.ResultProfile{ProfileID: "profile-1", ProfileVersion: "1.0.0", Digest: "sha256:" + strings.Repeat("3", 64)},
		Provider: Identity("provider", "provider-1", ""), ProviderJob: Identity("provider-job", "job-1", ""),
		Worker: Identity("worker", "worker-1", "4"), Environment: Identity("environment", "environment-1", "5"),
		Status: execution.ResultProvisional, ObservedOutcome: execution.ResultCompleted,
		Phases: []execution.ExecutionPhase{{
			Phase: "test", CommandID: "run-tests", Status: "succeeded", StartedAt: "2026-11-17T19:00:00Z", FinishedAt: "2026-11-17T19:01:00Z",
			Artifacts: []execution.Artifact{logReference},
		}},
		Tests: execution.TestExecutionEvidence{
			ExpectedTestIDs: []string{"example.SmokeTest#works"}, DiscoveredTestIDs: []string{"example.SmokeTest#works"},
			SelectedTestIDs: []string{"example.SmokeTest#works"}, ExecutedTestIDs: []string{"example.SmokeTest#works"}, SkippedTestIDs: []string{}, FailedTestIDs: []string{},
		},
		Artifacts: []execution.Artifact{logReference}, Cleanup: execution.CleanupEvidence{Status: "pending"},
		StartedAt: "2026-11-17T19:00:00Z", UpdatedAt: "2026-11-17T19:01:00Z",
	}, Content{Artifact: logReference, Bytes: logBytes}
}

func Identity(kind, id, digestSuffix string) execution.Identity {
	digest := ""
	if digestSuffix != "" {
		digest = "sha256:" + strings.Repeat(digestSuffix, 64)
	}
	return execution.Identity{Kind: kind, ID: id, Digest: digest}
}
