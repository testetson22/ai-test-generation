package localservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/agentworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/httpapi"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/localworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

const generationFixturePath = "src/test/java/example/GeneratedTest.java"

// These tests use real domain transitions and CAS bytes, but no runtime workers,
// database, HTTP listener, model SDK, source checkout, or Maven process.
func TestAutomaticReviewGateApprovesCheckpointsWithAttribution(t *testing.T) {
	h := newGenerationServiceFixture(t, true)
	h.config.API.ReviewGateMode = "automatic"
	h.service = h.restart(t)
	h.create(t)
	completed := h.terminal(t)
	if completed.Status != testgenflow.StatusCompleted || completed.Stage != testgenflow.StageDone || completed.TestsAdded != 1 || completed.ResultArtifactRef == "" {
		t.Fatalf("automatic gate did not complete the run: %#v", completed)
	}
	if len(completed.Reviews) != 2 {
		t.Fatalf("automatic gate must record both checkpoint decisions: %#v", completed.Reviews)
	}
	for _, review := range completed.Reviews {
		if review.DecisionMode != "automatic" || review.Action != "approve" || review.ReviewerID != h.config.API.ReviewerID || review.ReviewerRole != h.config.API.ReviewerRole || review.AuthorizationRef != h.config.API.ReviewAuthorizationRef || review.Rationale == "" {
			t.Fatalf("automatic decision lost attribution or mode marker: %#v", review)
		}
	}
	if !slices.Equal(completed.Reviews[1].ExpectedTestIDs, []string{"example.GeneratedTest#modelSuggested"}) || completed.Reviews[1].ApprovedTestArtifactDigest == "" {
		t.Fatalf("automatic code approval lost the model-suggested inventory or approved artifact: %#v", completed.Reviews[1])
	}
	if !slices.Equal(h.model.phases(), []string{"analyze", "plan", "code"}) || !slices.Equal(h.worker.phases(), []testgenflow.MavenPhase{testgenflow.MavenPhasePackage, testgenflow.MavenPhaseFocusedTests, testgenflow.MavenPhaseFinalVerify}) {
		t.Fatalf("automatic gate changed the staged phase order: model=%v worker=%v", h.model.phases(), h.worker.phases())
	}
}

func TestGenerationServiceReviewsResumeAcrossRestartAndPublishRetainedPatch(t *testing.T) {
	h := newGenerationServiceFixture(t, true)
	initial := h.create(t)
	plan := h.checkpoint(t, "plan")
	if plan.AttemptCount() != 1 || plan.TestsAdded != 0 || !slices.Equal(h.model.phases(), []string{"analyze", "plan"}) || !slices.Equal(h.worker.phases(), []testgenflow.MavenPhase{testgenflow.MavenPhasePackage}) {
		t.Fatalf("plan checkpoint executed unapproved work: run=%#v model=%v Maven=%v", plan, h.model.phases(), h.worker.phases())
	}
	if worked, err := h.service.DriveOnce(t.Context()); worked || err != nil {
		t.Fatalf("pending plan review should not advance: %t %v", worked, err)
	}
	h.service = h.restart(t)
	h.review(t, plan, "approve", nil)
	code := h.checkpoint(t, "code")
	if !slices.Equal(h.model.phases(), []string{"analyze", "plan", "code"}) || len(h.worker.materialized) != 0 || len(h.worker.maven) != 1 {
		t.Fatalf("restart reran phases or code preceded approval: phases=%v materialized=%v Maven=%v", h.model.phases(), h.worker.materialized, h.worker.phases())
	}
	if code.Checkpoint.ID == plan.Checkpoint.ID || code.Checkpoint.ArtifactRef == plan.Checkpoint.ArtifactRef || code.TestsAdded != 0 {
		t.Fatalf("code checkpoint lost independent scope: %#v", code)
	}
	analysis := h.model.jobs[h.model.calls[0].Work.IdempotencyKey].ArtifactRef
	if h.model.calls[0].AnalysisRef != "" || h.model.calls[1].AnalysisRef != analysis || h.model.calls[1].PlanRef != "" || h.model.calls[2].AnalysisRef != analysis || h.model.calls[2].PlanRef != plan.Checkpoint.ArtifactRef {
		t.Fatalf("phase dependencies lost retained artifact bindings: %#v", h.model.calls)
	}
	for _, call := range h.model.calls {
		if len(call.Work.Reviews) != 0 {
			t.Fatalf("review authority leaked into mutable model inputs: %#v", call.Work.Reviews)
		}
		if call.Work.RunID != initial.RunID || call.Work.ResolvedCommit != initial.Request.Revision || call.Work.RequestDigest != initial.RequestDigest || call.Work.Request.ProfileDigest != initial.Request.ProfileDigest || call.Work.Request.Generation.PolicyDigest != initial.Request.Generation.PolicyDigest {
			t.Fatalf("phase lost admitted source/profile/policy/request identity: %#v", call)
		}
	}
	body := h.reviewBody(code, "approve", nil)
	response := h.request(http.MethodPost, h.reviewPath(code), body)
	if response.Code != http.StatusBadRequest || len(h.worker.materialized) != 0 || h.run(t).Revision != code.Revision {
		t.Fatalf("missing human inventory admitted candidate: %d %s", response.Code, response.Body.String())
	}
	if worked, err := h.service.DriveOnce(t.Context()); worked || err != nil || len(h.worker.maven) != 1 {
		t.Fatalf("unreviewed code dispatched focused tests: %t %v", worked, err)
	}
	ids := []string{"example.GeneratedTest#humanSelected"}
	approved := h.review(t, code, "approve", ids)
	if len(approved.Reviews) != 2 || !slices.Equal(approved.Reviews[1].ExpectedTestIDs, ids) || approved.Reviews[1].ApprovedTestArtifactDigest == "" {
		t.Fatalf("code review lacks independent retained inventory: %#v", approved.Reviews)
	}
	raw := h.get(t, approved.Reviews[1].ApprovedTestArtifactDigest)
	candidate, err := localworker.DecodeTestArtifact(raw)
	if err != nil || !slices.Equal(candidate.ExpectedTestIDs, ids) || len(candidate.Files) != 1 || candidate.Files[0].Path != generationFixturePath {
		t.Fatalf("review did not retain canonical human-approved artifact: %#v %v", candidate, err)
	}
	h.service = h.restart(t)
	completed := h.terminal(t)
	if completed.RunID != initial.RunID || completed.Status != testgenflow.StatusCompleted || completed.Stage != testgenflow.StageDone || completed.TestsAdded != 1 || completed.PRURL != "" || completed.ResultArtifactRef == "" || completed.TestArtifactDigest != approved.Reviews[1].ApprovedTestArtifactDigest {
		t.Fatalf("reviewed patch-only result = %#v", completed)
	}
	if !bytes.Equal(h.get(t, completed.PatchDigest), candidate.PatchContent()) || len(h.worker.materialized) != 1 || h.worker.sourceCalls != 1 || !slices.Equal(h.worker.phases(), []testgenflow.MavenPhase{testgenflow.MavenPhasePackage, testgenflow.MavenPhaseFocusedTests, testgenflow.MavenPhaseFinalVerify}) {
		t.Fatalf("candidate, source, or execution bindings changed: worker=%#v", h.worker)
	}
	h.assertManifest(t, completed, 2, 1)
}

func TestGenerationServiceRejectsWithoutGeneratingOrExecutingLaterStages(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			h := newGenerationServiceFixture(t, true)
			h.create(t)
			checkpoint := h.checkpoint(t, "plan")
			if stage == "code" {
				h.review(t, checkpoint, "approve", nil)
				checkpoint = h.checkpoint(t, "code")
			}
			rejected := h.review(t, checkpoint, "reject", nil)
			before := slices.Clone(h.model.phases())
			if rejected.Status != testgenflow.StatusRejected || rejected.TestsAdded != 0 || rejected.ResultArtifactRef != "" {
				t.Fatalf("rejection became a passing result: %#v", rejected)
			}
			if worked, err := h.service.DriveOnce(t.Context()); worked || err != nil || !slices.Equal(before, h.model.phases()) || len(h.worker.maven) != 1 || len(h.worker.materialized) != 0 {
				t.Fatalf("rejected run dispatched work: %t %v model=%v Maven=%v", worked, err, h.model.phases(), h.worker.phases())
			}
			if stage == "plan" && !slices.Equal(before, []string{"analyze", "plan"}) {
				t.Fatalf("code ran before rejected plan approval: %v", before)
			}
		})
	}
}

func TestGenerationServicePreservesStatedUncertaintyWithoutLaterModelOrMavenWork(t *testing.T) {
	for _, phase := range []string{"analyze", "plan", "code"} {
		for _, status := range []string{"blocked", "inconclusive"} {
			t.Run(phase+"/"+status, func(t *testing.T) {
				h := newGenerationServiceFixture(t, true)
				h.model.status[phase] = status
				h.create(t)
				if phase == "code" {
					h.review(t, h.checkpoint(t, "plan"), "approve", nil)
				}
				run := h.terminal(t)
				wantPhases := map[string][]string{"analyze": {"analyze"}, "plan": {"analyze", "plan"}, "code": {"analyze", "plan", "code"}}[phase]
				if run.Status != testgenflow.StatusBlocked || run.GenerationOutcome != status || run.EvidenceRef == "" || run.LastError == "" || run.TestsAdded != 0 || run.ResultArtifactRef != "" || !slices.Equal(h.model.phases(), wantPhases) || len(h.worker.maven) != 1 || len(h.worker.materialized) != 0 {
					t.Fatalf("uncertainty converted to usable tests: run=%#v phases=%v Maven=%v", run, h.model.phases(), h.worker.phases())
				}
				var evidence map[string]any
				if err := json.Unmarshal(h.get(t, run.EvidenceRef), &evidence); err != nil || evidence["synthetic"] != true || evidence["status"] != status {
					t.Fatalf("stated uncertainty lost retained fake evidence: %#v %v", evidence, err)
				}
			})
		}
	}
}

func TestGenerationServiceDisabledGateMakesZeroReadinessOrStageCalls(t *testing.T) {
	h := newGenerationServiceFixture(t, false)
	if service, err := New(h.config); service != nil || !errors.Is(err, agentworker.ErrExternalOperationsDisabled) {
		t.Fatalf("closed gate admitted a model service: %#v %v", service, err)
	}
	if h.model.readyCalls != 0 || len(h.model.calls) != 0 || h.worker.readyCalls != 0 || h.worker.sourceCalls != 0 || len(h.worker.maven) != 0 || h.repository.modelClaims != 0 || len(h.repository.runs) != 0 {
		t.Fatalf("closed gate invoked a port or claimed work: model=%#v worker=%#v", h.model, h.worker)
	}
}

func TestGenerationServiceLongUncertaintyDiagnosticsKeepOutcomeAndEvidence(t *testing.T) {
	for _, status := range []string{"blocked", "inconclusive"} {
		for _, diagnostic := range []string{strings.Repeat("x", 2048), strings.Repeat("\u00e9", 1024)} {
			t.Run(status+fmt.Sprint(len(diagnostic)), func(t *testing.T) {
				h := newGenerationServiceFixture(t, true)
				h.model.status["analyze"], h.model.diagnostic = status, diagnostic
				h.create(t)
				run := h.terminal(t)
				if run.Status != testgenflow.StatusBlocked || run.GenerationOutcome != status || run.EvidenceRef == "" || len(run.LastError) > 1024 || !utf8.ValidString(run.LastError) || len(h.model.calls) != 1 || len(h.worker.maven) != 1 {
					t.Fatalf("long uncertainty lost meaning: %#v", run)
				}
				var retained map[string]any
				if err := json.Unmarshal(h.get(t, run.EvidenceRef), &retained); err != nil || retained["diagnostic"] != diagnostic {
					t.Fatalf("full diagnostic lost: %v", err)
				}
			})
		}
	}
}

func TestGenerationServiceClaimsOnlyActiveModelPolicyAndProfile(t *testing.T) {
	h := newGenerationServiceFixture(t, true)
	selected := h.create(t)
	for i, mutate := range []func(*testgenflow.Request){
		func(r *testgenflow.Request) { r.ProfileDigest = artifact.Digest([]byte("inactive-profile")) },
		func(r *testgenflow.Request) { r.Generation.PolicyDigest = artifact.Digest([]byte("inactive-policy")) },
		func(r *testgenflow.Request) {
			r.InputMode, r.Generation, r.InputArtifactDigest = testgenflow.InputModeArtifactInput, nil, artifact.Digest([]byte("input"))
		},
		func(r *testgenflow.Request) { r.InputMode, r.Generation, r.CompletionMode = "", nil, "" },
	} {
		request := generationClone(selected.Request)
		mutate(&request)
		run, err := testgenflow.NewRun(fmt.Sprintf("unselected-%d", i), fmt.Sprintf("other-%d", i), request, 2, selected.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := h.repository.CreateRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}
	h.drive(t)
	if h.run(t).Revision != 2 || h.repository.modelClaims != 1 || h.repository.artifactClaims != 0 || !slices.Equal(h.worker.sourceRunIDs, []string{selected.RunID}) || !slices.Equal(h.repository.lastFilter, []string{h.config.API.ProfileDigest, h.config.API.GenerationPolicyDigest}) {
		t.Fatalf("model driver selected wrong mode/profile/policy: filters=%v source=%v", h.repository.lastFilter, h.worker.sourceRunIDs)
	}
	for i := 0; i < 4; i++ {
		run, err := h.repository.GetRun(t.Context(), fmt.Sprintf("unselected-%d", i))
		if err != nil || run.Revision != 1 {
			t.Fatalf("unselected request advanced: %#v %v", run, err)
		}
	}
}

func TestGenerationServiceLostPhaseFinishAcknowledgementReplaysWithoutNewModelJob(t *testing.T) {
	for _, phase := range []string{"analyze", "plan", "code"} {
		t.Run(phase, func(t *testing.T) {
			h := newGenerationServiceFixture(t, true)
			h.create(t)
			if phase == "code" {
				h.review(t, h.checkpoint(t, "plan"), "approve", nil)
			} else {
				for range 3 {
					h.drive(t)
				}
			}
			h.repository.loseFinishKind = "generation:" + phase
			if worked, err := h.service.DriveOnce(t.Context()); worked || err != nil {
				t.Fatalf("lost finish acknowledgement should remain pending: %t %v", worked, err)
			}
			before := h.run(t)
			if before.Status != testgenflow.StatusRunning || before.Checkpoint != nil {
				t.Fatalf("unacknowledged phase was projected: %#v", before)
			}
			key := testgenflow.WorkKey(before.RunID, "generate", 1) + ":" + phase
			op := h.operation(t, key)
			if op.State != "completed" {
				t.Fatalf("ack loss lost retained operation: %#v", op)
			}
			var result agentworker.StageResult
			if err := json.Unmarshal(op.Result, &result); err != nil {
				t.Fatal(err)
			}
			h.get(t, result.ArtifactRef)
			h.service = h.restart(t)
			checkpoint := "plan"
			if phase == "code" {
				checkpoint = "code"
			}
			h.checkpoint(t, checkpoint)
			count := 0
			for _, call := range h.model.calls {
				if call.Phase == phase {
					count++
				}
			}
			if count != 1 || !reflect.DeepEqual(op, h.operation(t, key)) {
				t.Fatalf("completed phase replay reinvoked model or mutated operation: calls=%v", h.model.phases())
			}
		})
	}
}

func TestGenerationServiceReconcilesUnknownCompletedPlanWithoutModelResubmission(t *testing.T) {
	h := newGenerationServiceFixture(t, true)
	h.model.unknownAfterComplete = "plan"
	h.create(t)
	for range 10 {
		run := h.run(t)
		if run.Status == testgenflow.StatusOutcomeUnknown {
			break
		}
		worked, err := h.service.DriveOnce(t.Context())
		if err != nil || !worked {
			t.Fatalf("drive to unknown Plan result: worked=%t err=%v run=%#v", worked, err, run)
		}
	}
	unknown := h.run(t)
	if unknown.Status != testgenflow.StatusOutcomeUnknown || unknown.LastError != "generation evidence retained but completion acknowledgement unknown" {
		t.Fatalf("fixture did not produce the intended unknown Plan outcome: %#v", unknown)
	}
	key := testgenflow.WorkKey(unknown.RunID, "generate", 1) + ":plan"
	if operation := h.operation(t, key); operation.State != "unknown" {
		t.Fatalf("Plan operation was not left unknown: %#v", operation)
	}
	providerRequests := func() int {
		count := 0
		for _, call := range h.model.calls {
			if !call.RecoveryOnly {
				count++
			}
		}
		return count
	}
	if providerRequests() != 2 {
		t.Fatalf("unexpected number of initial model requests: %#v", h.model.calls)
	}
	stale := h.request(http.MethodPost, "/development/test-generation-runs/"+unknown.RunID+"/reconcile", map[string]any{"expected_revision": unknown.Revision - 1})
	if stale.Code != http.StatusConflict || !reflect.DeepEqual(h.run(t), unknown) || providerRequests() != 2 {
		t.Fatalf("stale recovery changed run or contacted model: %d %s", stale.Code, stale.Body.String())
	}
	response := h.request(http.MethodPost, "/development/test-generation-runs/"+unknown.RunID+"/reconcile", map[string]any{"expected_revision": unknown.Revision})
	var recovered httpapi.TestGenerationDevelopmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &recovered); err != nil || response.Code != http.StatusAccepted {
		t.Fatalf("reconcile completed Plan: %d %s %v", response.Code, response.Body.String(), err)
	}
	checkpoint := recovered.Run
	if checkpoint.Status != testgenflow.StatusWaitingReview || checkpoint.Revision != unknown.Revision+1 || checkpoint.Checkpoint == nil || checkpoint.Checkpoint.Stage != "plan" || checkpoint.Checkpoint.ArtifactRef != h.model.jobs[key].ArtifactRef || checkpoint.LastError != "" {
		t.Fatalf("recovery did not reach the exact Plan review checkpoint: %#v", checkpoint)
	}
	if operation := h.operation(t, key); operation.State != "completed" {
		t.Fatalf("recovered operation was not durably completed: %#v", operation)
	}
	if providerRequests() != 2 || len(h.model.calls) != 3 || !h.model.calls[2].RecoveryOnly || !h.model.calls[2].Work.ReconcileOnly || len(h.worker.maven) != 1 {
		t.Fatalf("recovery resubmitted generation or launched later execution: calls=%#v Maven=%v", h.model.calls, h.worker.phases())
	}
	approved := h.review(t, checkpoint, "approve", nil)
	if approved.Status != testgenflow.StatusRunning || approved.Checkpoint != nil || len(approved.Reviews) != 1 {
		t.Fatalf("recovered Plan did not enter normal human review flow: %#v", approved)
	}
}

func putRepairArtifact(t *testing.T, store *artifact.FileStore, data []byte) string {
	t.Helper()
	ref, err := store.Put(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	return ref.Digest
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func differentialOperationsFixture(t *testing.T) (*operations, *generationMemoryRepository, *artifact.FileStore) {
	t.Helper()
	store, err := artifact.NewFileStore(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repo := &generationMemoryRepository{runs: map[string]testgenflow.Run{}, events: map[string]map[string]string{}, operations: map[string]map[string]testgenflow.Operation{}, claims: map[string]testgenflow.FlowClaim{}}
	service := &Service{config: Config{Repository: repo, Artifacts: store, MaxTestArtifactBytes: 1 << 20}}
	return &operations{service: service}, repo, store
}

func TestPairRepairFeedbackBindsSubjectsAndIncludesOutcomes(t *testing.T) {
	o, repo, store := differentialOperationsFixture(t)
	profileDigest := "sha256:" + strings.Repeat("a", 64)
	baseCommit, candidateCommit := strings.Repeat("c", 40), strings.Repeat("d", 40)
	candidateJSON := `{"schema_version":"1","files":[{"path":"src/test/java/example/CveTest.java","content":"package example;\nclass CveTest {}\n"}],"expected_test_ids":["example.CveTest#verifies"]}`
	artifactDigest := putRepairArtifact(t, store, []byte(candidateJSON))
	logRef := putRepairArtifact(t, store, []byte("base subject command log tail\n"))
	baseEvidence := localworker.Evidence{SchemaVersion: "1", Subject: "base", SourceCommit: baseCommit, ProfileDigest: profileDigest, TestArtifactDigest: artifactDigest, Phase: "focused_tests", Succeeded: true, Artifacts: map[string]string{"command.log": logRef}}
	candidateEvidence := localworker.Evidence{SchemaVersion: "1", Subject: "candidate", SourceCommit: candidateCommit, ProfileDigest: profileDigest, TestArtifactDigest: artifactDigest, Phase: "focused_tests", Succeeded: true}
	baseRef := putRepairArtifact(t, store, mustMarshal(t, baseEvidence))
	subjectRef := putRepairArtifact(t, store, mustMarshal(t, candidateEvidence))
	pair := localworker.PairEvidence{SchemaVersion: "1", Phase: "focused_tests", RunID: "run-1", ProfileDigest: profileDigest, TestArtifactDigest: artifactDigest, Subjects: []localworker.SubjectOutcome{
		{Subject: "base", Commit: baseCommit, Succeeded: true, EvidenceRef: baseRef},
		{Subject: "candidate", Commit: candidateCommit, Succeeded: true, EvidenceRef: subjectRef},
	}, Succeeded: true}
	pairRef := putRepairArtifact(t, store, mustMarshal(t, pair))
	run := testgenflow.Run{RunID: "run-1", Request: testgenflow.Request{ProfileDigest: profileDigest}, ResolvedCommit: baseCommit, MaxAttempts: 2, Attempts: []testgenflow.GenerationAttempt{{Number: 1, Status: testgenflow.AttemptFailed, TestArtifactDigest: artifactDigest, FocusedTestEvidenceRef: pairRef, LastError: "differential did not match the approved plan oracle"}}}
	repo.runs["run-1"] = run
	work := testgenflow.WorkRequest{RunID: "run-1", Attempt: 2}

	feedback, err := o.repairFeedback(context.Background(), run, work)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"did not satisfy the approved oracle", "base@" + baseCommit + ": passed", "candidate@" + candidateCommit + ": passed", artifactDigest, pairRef, "base subject command log tail"} {
		if !strings.Contains(feedback, want) {
			t.Fatalf("pair repair feedback lost %q: %s", want, feedback)
		}
	}

	tampered := pair
	tampered.Subjects[0].Commit = candidateCommit
	if _, err = o.pairRepairFeedback(context.Background(), run, run.Attempts[0], tampered, func(data []byte, n int, tail bool) string { return string(data) }); err == nil || !strings.Contains(err.Error(), "does not bind") {
		t.Fatalf("tampered subject evidence accepted: %v", err)
	}
}

func TestGenerationServiceRepairRequiresFreshReviewsAndDistinctPhaseKeys(t *testing.T) {
	h := newGenerationServiceFixture(t, true)
	h.worker.focusedFailures = 1
	h.create(t)
	firstPlan := h.checkpoint(t, "plan")
	h.review(t, firstPlan, "approve", nil)
	firstCode := h.checkpoint(t, "code")
	h.review(t, firstCode, "approve", []string{"example.GeneratedTest#humanSelected"})
	secondPlan := h.checkpoint(t, "plan")
	if secondPlan.Checkpoint.Attempt != 2 || secondPlan.Checkpoint.ID == firstPlan.Checkpoint.ID || secondPlan.Attempts[0].Status != testgenflow.AttemptFailed || len(h.worker.maven) != 2 || !slices.Equal(h.model.phases(), []string{"analyze", "plan", "code", "analyze", "plan"}) {
		t.Fatalf("repair bypassed fresh plan review or reran baseline: %#v phases=%v Maven=%v", secondPlan, h.model.phases(), h.worker.phases())
	}
	for _, call := range h.model.calls {
		if call.Work.Attempt == 2 && (!strings.Contains(call.Work.RepairFeedback, secondPlan.Attempts[0].TestArtifactDigest) || !strings.Contains(call.Work.RepairFeedback, secondPlan.Attempts[0].FocusedTestEvidenceRef) || !strings.Contains(call.Work.RepairFeedback, generationFixturePath) || !strings.Contains(call.Work.RepairFeedback, "synthetic Maven diagnostic") || call.PreviousCandidateRef != firstCode.Checkpoint.ArtifactRef) {
			t.Fatalf("repair lost prior candidate or retained failure feedback: %#v", call)
		}
	}
	h.review(t, secondPlan, "approve", nil)
	secondCode := h.checkpoint(t, "code")
	if secondCode.Checkpoint.Attempt != 2 || secondCode.Checkpoint.ID == firstCode.Checkpoint.ID || len(h.worker.maven) != 2 {
		t.Fatalf("repair reused code review or dispatched unapproved focus: %#v", secondCode)
	}
	h.review(t, secondCode, "approve", []string{"example.GeneratedTest#humanSelected"})
	completed := h.terminal(t)
	if completed.Status != testgenflow.StatusCompleted || completed.AttemptCount() != 2 || len(completed.Reviews) != 4 || !slices.Equal(h.worker.phases(), []testgenflow.MavenPhase{testgenflow.MavenPhasePackage, testgenflow.MavenPhaseFocusedTests, testgenflow.MavenPhaseFocusedTests, testgenflow.MavenPhaseFinalVerify}) {
		t.Fatalf("repair failed to complete reviewed attempt: %#v phases=%v", completed, h.worker.phases())
	}
	keys := map[string]bool{}
	for _, call := range h.model.calls {
		if keys[call.Work.IdempotencyKey] {
			t.Fatalf("phase identity reused: %s", call.Work.IdempotencyKey)
		}
		keys[call.Work.IdempotencyKey] = true
	}
	if len(keys) != 6 {
		t.Fatalf("expected six independent attempt/phase identities, got %v", keys)
	}
	h.assertManifest(t, completed, 4, 2)
}

func TestGenerationServiceMissingPreparedModelJobMakesCancellationUnknown(t *testing.T) {
	h := newGenerationServiceFixture(t, true)
	h.create(t)
	run := h.checkpoint(t, "plan")
	claim, err := h.repository.ClaimNextModelGenerationRun(t.Context(), "seed", run.Request.ProfileDigest, run.Request.Generation.PolicyDigest, time.Minute)
	if err != nil || claim != nil {
		t.Fatalf("waiting review was claimable: %#v %v", claim, err)
	}
	// Plan approval authorizes preparing code, but a lost create leaves an
	// intent without a confirmable model job. Cancellation cannot infer exit.
	run = h.review(t, run, "approve", nil)
	claim, err = h.repository.ClaimNextModelGenerationRun(t.Context(), "seed", run.Request.ProfileDigest, run.Request.Generation.PolicyDigest, time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim for prepared intent: %#v %v", claim, err)
	}
	key := testgenflow.WorkKey(run.RunID, "generate", 1) + ":code"
	if _, _, err := h.repository.BeginTestGenerationOperation(t.Context(), *claim, key, "generation:code", artifact.Digest([]byte("prepared missing synthetic model job"))); err != nil {
		t.Fatal(err)
	}
	if err := h.repository.ReleaseTestGenerationClaim(t.Context(), *claim); err != nil {
		t.Fatal(err)
	}
	response := h.request(http.MethodPost, "/development/test-generation-runs/"+run.RunID+"/cancel", map[string]any{"expected_revision": run.Revision})
	if response.Code != http.StatusAccepted {
		t.Fatalf("cancel request: %d %s", response.Code, response.Body.String())
	}
	h.drive(t)
	unknown := h.run(t)
	if unknown.Status != testgenflow.StatusOutcomeUnknown || unknown.EvidenceRef == "" || h.model.cancelCalls != 0 || len(h.worker.maven) != 1 || len(h.model.calls) != 2 || !slices.Equal(h.model.confirmed, []string{key}) {
		t.Fatalf("missing model intent was falsely confirmed cancelled: run=%#v confirmed=%v", unknown, h.model.confirmed)
	}
	var partial map[string]any
	if err := json.Unmarshal(h.get(t, unknown.EvidenceRef), &partial); err != nil || partial["termination_confirmed"] != false {
		t.Fatalf("partial cancellation claimed complete: %#v %v", partial, err)
	}
	if op := h.operation(t, run.RunID+"/cancel"); op.State != "unknown" {
		t.Fatalf("unknown cancellation was not durably journaled: %#v", op)
	}
}

func TestGenerationServiceMemoryLedgerBindsPayloadsAndRejectsStaleFences(t *testing.T) {
	h := newGenerationServiceFixture(t, true)
	initial := h.create(t)
	copy := h.run(t)
	copy.Request.Generation.Objective = "must not mutate persisted request"
	if h.run(t).Request.Generation.Objective != initial.Request.Generation.Objective {
		t.Fatal("memory repository shares request pointers")
	}
	claim, err := h.repository.ClaimNextModelGenerationRun(t.Context(), "first-holder", initial.Request.ProfileDigest, initial.Request.Generation.PolicyDigest, time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim: %#v %v", claim, err)
	}
	if err := h.repository.ReleaseTestGenerationClaim(t.Context(), *claim); err != nil {
		t.Fatal(err)
	}
	next, err := h.repository.ClaimNextModelGenerationRun(t.Context(), "next-holder", initial.Request.ProfileDigest, initial.Request.Generation.PolicyDigest, time.Minute)
	if err != nil || next == nil || next.Fence <= claim.Fence {
		t.Fatalf("claim fence did not advance: %#v %v", next, err)
	}
	if _, _, err := h.repository.BeginTestGenerationOperation(t.Context(), *claim, "stale", "source", artifact.Digest([]byte("input"))); !errors.Is(err, testgenflow.ErrFlowClaimLost) {
		t.Fatalf("stale holder prepared work: %v", err)
	}
	event := testgenflow.Event{ID: "memory-source-event", ExpectedRevision: initial.Revision, Type: testgenflow.EventSourceResolved, ResolvedCommit: initial.Request.Revision, OccurredAt: initial.CreatedAt}
	if _, err := h.repository.ApplyClaimedTestGenerationEvent(t.Context(), *claim, event); !errors.Is(err, testgenflow.ErrFlowClaimLost) {
		t.Fatalf("stale holder applied event: %v", err)
	}
	applied, err := h.repository.ApplyClaimedTestGenerationEvent(t.Context(), *next, event)
	if err != nil || applied.Revision != 2 {
		t.Fatalf("apply real domain event: %#v %v", applied, err)
	}
	replayed, err := h.repository.ApplyEvent(t.Context(), initial.RunID, event)
	if err != nil || !reflect.DeepEqual(applied, replayed) {
		t.Fatalf("identical event did not replay: %#v %v", replayed, err)
	}
	event.ResolvedCommit = strings.Repeat("b", 40)
	if _, err := h.repository.ApplyEvent(t.Context(), initial.RunID, event); !errors.Is(err, testgenflow.ErrEventConflict) {
		t.Fatalf("changed payload replay was accepted: %v", err)
	}
	if _, created, err := h.repository.BeginTestGenerationOperation(t.Context(), *next, "bound", "source", artifact.Digest([]byte("input"))); err != nil || !created {
		t.Fatalf("prepare: %t %v", created, err)
	}
	if _, _, err := h.repository.BeginTestGenerationOperation(t.Context(), *next, "bound", "source", artifact.Digest([]byte("different input"))); !errors.Is(err, testgenflow.ErrOperationConflict) {
		t.Fatalf("operation input was not bound: %v", err)
	}
	if err := h.repository.FinishTestGenerationOperation(t.Context(), *next, "bound", "completed", json.RawMessage(`"retained fixture"`), ""); err != nil {
		t.Fatal(err)
	}
	if err := h.repository.FinishTestGenerationOperation(t.Context(), *next, "bound", "completed", json.RawMessage(`"changed outcome"`), ""); !errors.Is(err, testgenflow.ErrOperationConflict) {
		t.Fatalf("completed operation was overwritten: %v", err)
	}
}

func TestGenerationServiceCancellationRequiresBothComponentConfirmations(t *testing.T) {
	for _, unconfirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("model_termination_unconfirmed_%t", unconfirmed), func(t *testing.T) {
			h := newGenerationServiceFixture(t, true)
			h.model.unconfirmedCancellation = unconfirmed
			h.create(t)
			run := h.checkpoint(t, "plan")
			response := h.request(http.MethodPost, "/development/test-generation-runs/"+run.RunID+"/cancel", map[string]any{"expected_revision": run.Revision})
			if response.Code != http.StatusAccepted {
				t.Fatalf("cancellation request: %d %s", response.Code, response.Body.String())
			}
			h.drive(t)
			result := h.run(t)
			if len(h.model.calls) != 2 || len(h.worker.maven) != 1 || h.model.cancelCalls != 1 || h.worker.cancelCalls != 1 {
				t.Fatalf("cancellation dispatched model/Maven or omitted a component: model=%v Maven=%v", h.model.phases(), h.worker.phases())
			}
			h.get(t, h.model.cancelRef)
			h.get(t, h.worker.cancelRef)
			if unconfirmed {
				if result.Status != testgenflow.StatusOutcomeUnknown || result.EvidenceRef == "" || h.model.cleanupCalls != 0 || h.worker.cleanupCalls != 0 {
					t.Fatalf("partial confirmation became successful cancellation: %#v", result)
				}
				return
			}
			var manifest struct {
				Confirmed  bool     `json:"termination_confirmed"`
				Components []string `json:"component_manifest_refs"`
			}
			if err := json.Unmarshal(h.get(t, result.EvidenceRef), &manifest); err != nil {
				t.Fatal(err)
			}
			if result.Status != testgenflow.StatusCancelled || !manifest.Confirmed || !slices.Equal(manifest.Components, []string{h.worker.cancelRef, h.model.cancelRef}) || h.worker.cleanupCalls != 1 || h.model.cleanupCalls != 1 {
				t.Fatalf("cancellation lost separate confirmations or cleanup: run=%#v manifest=%#v", result, manifest)
			}
		})
	}
}

func TestGenerationServiceCancelsModelDespiteExecutionCancellationErrors(t *testing.T) {
	for _, phase := range []string{"confirm", "cancel"} {
		t.Run(phase, func(t *testing.T) {
			h := newGenerationServiceFixture(t, true)
			h.create(t)
			run := h.review(t, h.checkpoint(t, "plan"), "approve", nil)
			claim, err := h.repository.ClaimNextModelGenerationRun(t.Context(), "active-code-fixture", run.Request.ProfileDigest, run.Request.Generation.PolicyDigest, time.Minute)
			if err != nil || claim == nil {
				t.Fatalf("claim: %#v %v", claim, err)
			}
			key := testgenflow.WorkKey(run.RunID, "generate", 1) + ":code"
			if _, _, err := h.repository.BeginTestGenerationOperation(t.Context(), *claim, key, "generation:code", artifact.Digest([]byte("synthetic accepted active model job"))); err != nil {
				t.Fatal(err)
			}
			h.model.jobs[key] = agentworker.StageResult{Status: "running"}
			h.model.active = true
			if err := h.repository.ReleaseTestGenerationClaim(t.Context(), *claim); err != nil {
				t.Fatal(err)
			}
			if phase == "confirm" {
				h.worker.confirmError = errors.New("synthetic execution inspection unavailable")
			} else {
				h.worker.cancelError = errors.New("synthetic execution receipt unavailable")
			}
			response := h.request(http.MethodPost, "/development/test-generation-runs/"+run.RunID+"/cancel", map[string]any{"expected_revision": run.Revision})
			if response.Code != http.StatusAccepted {
				t.Fatalf("cancel request: %d %s", response.Code, response.Body.String())
			}
			h.drive(t)
			result := h.run(t)
			if result.Status != testgenflow.StatusOutcomeUnknown || result.EvidenceRef == "" || h.model.cancelCalls != 1 || h.model.active || h.model.cleanupCalls != 0 || h.worker.cleanupCalls != 0 {
				t.Fatalf("one component error prevented model termination or claimed success: %#v model=%#v", result, h.model)
			}
			var partial map[string]any
			if err := json.Unmarshal(h.get(t, result.EvidenceRef), &partial); err != nil || partial["termination_confirmed"] != false {
				t.Fatalf("incomplete termination claimed success: %#v %v", partial, err)
			}
		})
	}
}

type generationServiceFixture struct {
	repository *generationMemoryRepository
	files      *artifact.FileStore
	worker     *generationFakeExecution
	model      *generationFakeModel
	config     Config
	service    *Service
	runID      string
}

func newGenerationServiceFixture(t *testing.T, allow bool) *generationServiceFixture {
	t.Helper()
	files, err := artifact.NewFileStore(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	repo := &generationMemoryRepository{runs: map[string]testgenflow.Run{}, events: map[string]map[string]string{}, operations: map[string]map[string]testgenflow.Operation{}, claims: map[string]testgenflow.FlowClaim{}}
	worker := &generationFakeExecution{files: files}
	model := &generationFakeModel{files: files, policy: artifact.Digest([]byte("synthetic generation policy")), status: map[string]string{}, jobs: map[string]agentworker.StageResult{}}
	model.sourceEvidence = worker.RunEvidence
	now := time.Date(2026, 11, 23, 18, 0, 0, 123456000, time.UTC)
	config := Config{Repository: repo, Artifacts: files, Worker: worker, Generation: model, AllowModelCalls: allow,
		LeaseDuration: time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ValidateTestArtifact: func(raw []byte) error { _, err := localworker.DecodeTestArtifact(raw); return err },
		API:                  httpapi.TestGenerationDevelopmentConfig{BearerToken: strings.Repeat("synthetic-test-token", 2), RepositoryURL: "https://git.example.invalid/synthetic.git", PinnedRevision: strings.Repeat("a", 40), DefaultBaseBranch: "main", Subdirectory: "fixture", ProfileID: "synthetic-java-profile", ProfileVersion: "1.0.0", ProfileDigest: artifact.Digest([]byte("synthetic Java execution profile")), MaxAttempts: 2, InputMode: testgenflow.InputModeModelGeneration, CompletionMode: testgenflow.CompletionModePatchOnly, GenerationPolicyDigest: model.policy, GenerationContextPaths: []string{"pom.xml"}, ReviewerID: "human-fixture-reviewer", ReviewerRole: "reviewer", ReviewAuthorizationRef: "authz:synthetic-test", Now: func() time.Time { return now }}}
	h := &generationServiceFixture{repository: repo, files: files, worker: worker, model: model, config: config}
	if allow {
		h.service = h.restart(t)
	}
	return h
}

func (h *generationServiceFixture) restart(t *testing.T) *Service {
	t.Helper()
	service, err := New(h.config)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
func (h *generationServiceFixture) request(method, route string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, route, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+h.config.API.BearerToken)
	r.Header.Set("Idempotency-Key", "synthetic-generation-request")
	recorder := httptest.NewRecorder()
	h.service.Handler().ServeHTTP(recorder, r)
	return recorder
}
func (h *generationServiceFixture) create(t *testing.T) testgenflow.Run {
	t.Helper()
	response := h.request(http.MethodPost, "/development/test-generation-runs", map[string]any{"objective": "Synthetically test the stated behavior; do not run any external process."})
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var result httpapi.TestGenerationDevelopmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	h.runID = result.Run.RunID
	return result.Run
}
func (h *generationServiceFixture) run(t *testing.T) testgenflow.Run {
	t.Helper()
	run, err := h.repository.GetRun(t.Context(), h.runID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}
func (h *generationServiceFixture) drive(t *testing.T) {
	t.Helper()
	if worked, err := h.service.DriveOnce(t.Context()); !worked || err != nil {
		t.Fatalf("drive worked=%t err=%v run=%#v", worked, err, h.run(t))
	}
}
func (h *generationServiceFixture) checkpoint(t *testing.T, stage string) testgenflow.Run {
	t.Helper()
	for range 20 {
		run := h.run(t)
		if run.Status == testgenflow.StatusWaitingReview {
			if run.Checkpoint == nil || run.Checkpoint.Stage != stage {
				t.Fatalf("wanted %s review, got %#v", stage, run)
			}
			return run
		}
		h.drive(t)
	}
	t.Fatalf("%s checkpoint not reached: %#v", stage, h.run(t))
	return testgenflow.Run{}
}
func (h *generationServiceFixture) terminal(t *testing.T) testgenflow.Run {
	t.Helper()
	for range 20 {
		run := h.run(t)
		if run.Status != testgenflow.StatusQueued && run.Status != testgenflow.StatusRunning && run.Status != testgenflow.StatusCancelRequested {
			return run
		}
		h.drive(t)
	}
	t.Fatalf("terminal state not reached: %#v", h.run(t))
	return testgenflow.Run{}
}
func (h *generationServiceFixture) reviewPath(run testgenflow.Run) string {
	return "/development/test-generation-runs/" + run.RunID + "/reviews"
}
func (h *generationServiceFixture) reviewBody(run testgenflow.Run, action string, ids []string) map[string]any {
	return map[string]any{"expected_revision": run.Revision, "decision_id": fmt.Sprintf("human-%s-%d", run.Checkpoint.Stage, run.Checkpoint.Attempt), "checkpoint_id": run.Checkpoint.ID, "stage": run.Checkpoint.Stage, "artifact_ref": run.Checkpoint.ArtifactRef, "action": action, "rationale": "Independent synthetic human review, not a model authorization.", "expected_test_ids": ids}
}
func (h *generationServiceFixture) review(t *testing.T, run testgenflow.Run, action string, ids []string) testgenflow.Run {
	t.Helper()
	response := h.request(http.MethodPost, h.reviewPath(run), h.reviewBody(run, action, ids))
	if response.Code != http.StatusAccepted {
		t.Fatalf("review %s: %d %s", run.Checkpoint.Stage, response.Code, response.Body.String())
	}
	return h.run(t)
}
func (h *generationServiceFixture) get(t *testing.T, ref string) []byte {
	t.Helper()
	raw, err := h.files.Get(t.Context(), ref, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func (h *generationServiceFixture) operation(t *testing.T, key string) testgenflow.Operation {
	t.Helper()
	ops, err := h.repository.ListTestGenerationOperations(t.Context(), h.runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Key == key {
			return op
		}
	}
	t.Fatalf("operation %s not retained: %#v", key, ops)
	return testgenflow.Operation{}
}
func (h *generationServiceFixture) assertManifest(t *testing.T, run testgenflow.Run, reviews, attempt int) {
	t.Helper()
	var manifest struct {
		Mode               string                         `json:"mode"`
		RunID              string                         `json:"run_id"`
		PatchDigest        string                         `json:"patch_digest"`
		TestArtifactDigest string                         `json:"test_artifact_digest"`
		Cleanup            bool                           `json:"cleanup_confirmed"`
		AcceptedAttempt    int                            `json:"accepted_attempt"`
		Operations         []testgenflow.Operation        `json:"operations"`
		Reviews            []testgenflow.GenerationReview `json:"reviews"`
		Evidence           []string                       `json:"worker_evidence"`
	}
	if err := json.Unmarshal(h.get(t, run.ResultArtifactRef), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Mode != "model_generation_patch_only" || manifest.RunID != run.RunID || manifest.PatchDigest != run.PatchDigest || manifest.TestArtifactDigest != run.TestArtifactDigest || !manifest.Cleanup || manifest.AcceptedAttempt != attempt || len(manifest.Reviews) != reviews || !reflect.DeepEqual(manifest.Reviews, run.Reviews) {
		t.Fatalf("manifest lost approved provenance: %#v", manifest)
	}
	phases := map[string]bool{}
	for _, op := range manifest.Operations {
		switch op.Kind {
		case "source", "generation:analyze", "generation:plan", "generation:code", "generation:materialize", "maven:package", "maven:focused_tests", "maven:verify":
		default:
			t.Fatalf("unexpected operation, including any SCM operation, in patch-only result: %#v", op)
		}
		if op.State != "completed" || op.RunID != run.RunID || op.InputDigest == "" {
			t.Fatalf("unresolved/unbound/SCM operation in patch-only result: %#v", op)
		}
		phases[op.Kind] = true
		if op.Kind == "generation:analyze" || op.Kind == "generation:plan" || op.Kind == "generation:code" {
			var phase agentworker.StageResult
			if err := json.Unmarshal(op.Result, &phase); err != nil || phase.Status != "completed" {
				t.Fatalf("lost phase result: %#v %v", phase, err)
			}
			h.get(t, phase.ArtifactRef)
		}
		if strings.HasPrefix(op.Kind, "maven:") {
			var result testgenflow.MavenResult
			if err := json.Unmarshal(op.Result, &result); err != nil {
				t.Fatal(err)
			}
			var fake struct {
				Synthetic bool   `json:"synthetic"`
				Component string `json:"component"`
			}
			if err := json.Unmarshal(h.get(t, result.EvidenceRef), &fake); err != nil || !fake.Synthetic || fake.Component != "fake_maven" {
				t.Fatalf("Maven evidence is not retained explicit fake evidence: %#v %v", fake, err)
			}
		}
	}
	for _, kind := range []string{"source", "generation:analyze", "generation:plan", "generation:code", "generation:materialize", "maven:package", "maven:focused_tests", "maven:verify"} {
		if !phases[kind] {
			t.Fatalf("root manifest missing %s", kind)
		}
	}
	for _, review := range manifest.Reviews {
		if review.ReviewerID != h.config.API.ReviewerID || review.ReviewerRole != "reviewer" || review.AuthorizationRef != h.config.API.ReviewAuthorizationRef || review.Rationale == "" {
			t.Fatalf("unattributable review: %#v", review)
		}
		h.get(t, review.ArtifactRef)
	}
	if len(manifest.Evidence) != len(h.model.calls)+1 {
		t.Fatalf("root lost phase/source evidence: %v", manifest.Evidence)
	}
	for _, ref := range manifest.Evidence {
		h.get(t, ref)
	}
}

// JSON round trips intentionally prevent tests from sharing projection pointers.
func generationClone[T any](value T) T {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var copy T
	if err := json.Unmarshal(raw, &copy); err != nil {
		panic(err)
	}
	return copy
}

type generationMemoryRepository struct {
	mu                          sync.Mutex
	runs                        map[string]testgenflow.Run
	events                      map[string]map[string]string
	operations                  map[string]map[string]testgenflow.Operation
	claims                      map[string]testgenflow.FlowClaim
	fence                       int64
	loseFinishKind              string
	modelClaims, artifactClaims int
	lastFilter                  []string
}

var _ Repository = (*generationMemoryRepository)(nil)
var _ modelGenerationClaims = (*generationMemoryRepository)(nil)

func (r *generationMemoryRepository) Ping(context.Context) error { return nil }

func (r *generationMemoryRepository) CreateRun(_ context.Context, run testgenflow.Run) (testgenflow.Run, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := run.Validate(); err != nil {
		return testgenflow.Run{}, false, err
	}
	for _, previous := range r.runs {
		if previous.IdempotencyKey == run.IdempotencyKey {
			if previous.RequestDigest != run.RequestDigest {
				return testgenflow.Run{}, false, testgenflow.ErrIdempotencyConflict
			}
			return generationClone(previous), false, nil
		}
	}
	if _, exists := r.runs[run.RunID]; exists {
		return testgenflow.Run{}, false, testgenflow.ErrIdempotencyConflict
	}
	r.runs[run.RunID] = generationClone(run)
	return generationClone(run), true, nil
}
func (r *generationMemoryRepository) GetRun(_ context.Context, id string) (testgenflow.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[id]
	if !ok {
		return testgenflow.Run{}, testgenflow.ErrRunNotFound
	}
	return generationClone(run), nil
}
func (r *generationMemoryRepository) apply(id string, event testgenflow.Event) (testgenflow.Run, error) {
	run, ok := r.runs[id]
	if !ok {
		return testgenflow.Run{}, testgenflow.ErrRunNotFound
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return testgenflow.Run{}, err
	}
	payloadDigest := artifact.Digest(raw)
	if prior, exists := r.events[id][event.ID]; exists {
		if prior != payloadDigest {
			return testgenflow.Run{}, testgenflow.ErrEventConflict
		}
		return generationClone(run), nil
	}
	next, err := run.Apply(event)
	if err != nil {
		return testgenflow.Run{}, err
	}
	if r.events[id] == nil {
		r.events[id] = map[string]string{}
	}
	r.events[id][event.ID], r.runs[id] = payloadDigest, generationClone(next)
	return generationClone(next), nil
}
func (r *generationMemoryRepository) ApplyEvent(_ context.Context, id string, event testgenflow.Event) (testgenflow.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.apply(id, event)
}
func (r *generationMemoryRepository) claim(holder, profile, policy, mode string, lease time.Duration) (*testgenflow.FlowClaim, error) {
	ids := make([]string, 0, len(r.runs))
	for id := range r.runs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		run := r.runs[id]
		if run.Request.ProfileDigest != profile || run.Request.InputMode != mode || mode == testgenflow.InputModeModelGeneration && (run.Request.Generation == nil || run.Request.Generation.PolicyDigest != policy) || run.Status != testgenflow.StatusQueued && run.Status != testgenflow.StatusRunning && run.Status != testgenflow.StatusCancelRequested {
			continue
		}
		if current, ok := r.claims[id]; ok && current.LeaseUntil.After(time.Now()) {
			continue
		}
		r.fence++
		claim := testgenflow.FlowClaim{RunID: id, Holder: holder, Fence: r.fence, LeaseUntil: time.Now().Add(lease)}
		r.claims[id] = claim
		return &claim, nil
	}
	return nil, nil
}
func (r *generationMemoryRepository) ClaimNextTestGenerationRun(_ context.Context, holder, profile string, lease time.Duration) (*testgenflow.FlowClaim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.artifactClaims++
	return r.claim(holder, profile, "", testgenflow.InputModeArtifactInput, lease)
}
func (r *generationMemoryRepository) ClaimNextModelGenerationRun(_ context.Context, holder, profile, policy string, lease time.Duration) (*testgenflow.FlowClaim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modelClaims++
	r.lastFilter = []string{profile, policy}
	return r.claim(holder, profile, policy, testgenflow.InputModeModelGeneration, lease)
}
func (r *generationMemoryRepository) ClaimUnknownModelGenerationRun(_ context.Context, id, holder, profile, policy string, revision int64, lease time.Duration) (*testgenflow.FlowClaim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, exists := r.runs[id]
	if !exists || run.Status != testgenflow.StatusOutcomeUnknown || run.Revision != revision || run.Request.ProfileDigest != profile || run.Request.InputMode != testgenflow.InputModeModelGeneration || run.Request.Generation == nil || run.Request.Generation.PolicyDigest != policy {
		return nil, nil
	}
	if current, ok := r.claims[id]; ok && current.LeaseUntil.After(time.Now()) {
		return nil, nil
	}
	r.fence++
	claim := testgenflow.FlowClaim{RunID: id, Holder: holder, Fence: r.fence, LeaseUntil: time.Now().Add(lease)}
	r.claims[id] = claim
	return &claim, nil
}
func (r *generationMemoryRepository) fenced(claim testgenflow.FlowClaim) error {
	current, ok := r.claims[claim.RunID]
	if !ok || current.Holder != claim.Holder || current.Fence != claim.Fence || !current.LeaseUntil.After(time.Now()) {
		return testgenflow.ErrFlowClaimLost
	}
	return nil
}
func (r *generationMemoryRepository) RenewTestGenerationClaim(_ context.Context, claim testgenflow.FlowClaim, lease time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fenced(claim); err != nil {
		return err
	}
	claim.LeaseUntil = time.Now().Add(lease)
	r.claims[claim.RunID] = claim
	return nil
}
func (r *generationMemoryRepository) ReleaseTestGenerationClaim(_ context.Context, claim testgenflow.FlowClaim) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fenced(claim); err != nil {
		return err
	}
	delete(r.claims, claim.RunID)
	return nil
}
func (r *generationMemoryRepository) ApplyClaimedTestGenerationEvent(_ context.Context, claim testgenflow.FlowClaim, event testgenflow.Event) (testgenflow.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fenced(claim); err != nil {
		return testgenflow.Run{}, err
	}
	return r.apply(claim.RunID, event)
}
func (r *generationMemoryRepository) BeginTestGenerationOperation(_ context.Context, claim testgenflow.FlowClaim, key, kind, input string) (testgenflow.Operation, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fenced(claim); err != nil {
		return testgenflow.Operation{}, false, err
	}
	if op, exists := r.operations[claim.RunID][key]; exists {
		if op.Kind != kind || op.InputDigest != input {
			return testgenflow.Operation{}, false, testgenflow.ErrOperationConflict
		}
		return generationClone(op), false, nil
	}
	if r.operations[claim.RunID] == nil {
		r.operations[claim.RunID] = map[string]testgenflow.Operation{}
	}
	now := time.Now().UTC()
	op := testgenflow.Operation{RunID: claim.RunID, Key: key, Kind: kind, InputDigest: input, State: "prepared", CreatedAt: now, UpdatedAt: now}
	r.operations[claim.RunID][key] = op
	return generationClone(op), true, nil
}
func (r *generationMemoryRepository) FinishTestGenerationOperation(_ context.Context, claim testgenflow.FlowClaim, key, state string, result json.RawMessage, diagnostic string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fenced(claim); err != nil {
		return err
	}
	op, exists := r.operations[claim.RunID][key]
	if !exists {
		return testgenflow.ErrOperationConflict
	}
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	if !json.Valid(result) {
		return testgenflow.ErrOperationConflict
	}
	if op.State != "prepared" && !(op.State == "unknown" && state != "unknown") {
		if op.State != state || !bytes.Equal(op.Result, result) || op.Diagnostic != diagnostic {
			return testgenflow.ErrOperationConflict
		}
		return nil
	}
	op.State, op.Result, op.Diagnostic, op.UpdatedAt = state, slices.Clone(result), diagnostic, time.Now().UTC()
	r.operations[claim.RunID][key] = op
	if r.loseFinishKind == op.Kind {
		r.loseFinishKind = ""
		return errors.New("synthetic lost finish acknowledgement after durable save")
	}
	return nil
}
func (r *generationMemoryRepository) ListTestGenerationOperations(_ context.Context, id string) ([]testgenflow.Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ops := []testgenflow.Operation{}
	for _, op := range r.operations[id] {
		ops = append(ops, generationClone(op))
	}
	slices.SortFunc(ops, func(a, b testgenflow.Operation) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.Key, b.Key)
	})
	return ops, nil
}

func generationPut(ctx context.Context, store artifact.Store, value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	ref, err := store.Put(ctx, raw)
	if err == nil {
		_, err = store.Get(ctx, ref.Digest, 32<<20)
	}
	return ref.Digest, err
}

type generationFakeExecution struct {
	files                                    artifact.Store
	readyCalls, sourceCalls, focusedFailures int
	sourceRunIDs                             []string
	sourceEvidence                           string
	maven                                    []testgenflow.MavenWorkRequest
	materialized                             []testgenflow.WorkRequest
	cancelCalls, cleanupCalls                int
	cancelRef                                string
	cancelError, confirmError                error
}

func (w *generationFakeExecution) Ready(context.Context) error { w.readyCalls++; return nil }
func (w *generationFakeExecution) ResolveSource(ctx context.Context, work testgenflow.WorkRequest) (string, error) {
	w.sourceCalls++
	w.sourceRunIDs = append(w.sourceRunIDs, work.RunID)
	ref, err := generationPut(ctx, w.files, map[string]any{"synthetic": true, "component": "fake_source", "run_id": work.RunID, "commit": work.Request.Revision})
	w.sourceEvidence = ref
	return work.Request.Revision, err
}
func (w *generationFakeExecution) PrepareTestArtifact(ctx context.Context, work testgenflow.WorkRequest) (testgenflow.GenerationResult, error) {
	w.materialized = append(w.materialized, generationClone(work))
	raw, err := w.files.Get(ctx, work.TestArtifactDigest, localworker.MaxTestArtifactBytes)
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	candidate, err := localworker.DecodeTestArtifact(raw)
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	patch, err := w.files.Put(ctx, candidate.PatchContent())
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	if _, err := w.files.Get(ctx, patch.Digest, 32<<20); err != nil {
		return testgenflow.GenerationResult{}, err
	}
	paths := make([]string, len(candidate.Files))
	for i, file := range candidate.Files {
		paths[i] = file.Path
	}
	return testgenflow.GenerationResult{ChangedTestPaths: paths, TestArtifactDigest: work.TestArtifactDigest, PatchDigest: patch.Digest}, nil
}
func (w *generationFakeExecution) RunMaven(ctx context.Context, work testgenflow.MavenWorkRequest) (testgenflow.MavenResult, error) {
	w.maven = append(w.maven, generationClone(work))
	succeeded := true
	if work.Phase == testgenflow.MavenPhaseFocusedTests && w.focusedFailures > 0 {
		succeeded = false
		w.focusedFailures--
	}
	log, err := w.files.Put(ctx, []byte("synthetic Maven diagnostic for an explicit in-memory fixture\n"))
	if err != nil {
		return testgenflow.MavenResult{}, err
	}
	ref, err := generationPut(ctx, w.files, map[string]any{
		"synthetic": true, "component": "fake_maven", "run_id": work.RunID, "phase": work.Phase, "attempt": work.Attempt, "succeeded": succeeded,
		"source_commit": work.ResolvedCommit, "profile_digest": work.Request.ProfileDigest, "test_artifact_digest": work.TestArtifactDigest,
		"partial": false, "artifacts": map[string]string{"command.log": log.Digest},
	})
	diagnostic := ""
	if !succeeded {
		diagnostic = "synthetic focused failure with retained fake evidence"
	}
	return testgenflow.MavenResult{Succeeded: succeeded, EvidenceRef: ref, Diagnostic: diagnostic}, err
}
func (w *generationFakeExecution) RunEvidence(context.Context, string) ([]string, error) {
	if w.sourceEvidence == "" {
		return nil, nil
	}
	return []string{w.sourceEvidence}, nil
}
func (w *generationFakeExecution) ConfirmOperations(context.Context, string, []string) error {
	return w.confirmError
}
func (w *generationFakeExecution) CleanupRun(context.Context, string) error {
	w.cleanupCalls++
	w.sourceEvidence = ""
	return nil
}
func (w *generationFakeExecution) CancelRun(ctx context.Context, runID string) (string, error) {
	w.cancelCalls++
	if w.cancelError != nil {
		return "", w.cancelError
	}
	ref, err := generationPut(ctx, w.files, map[string]any{"synthetic": true, "component": "fake_execution_cancel", "run_id": runID, "termination_confirmed": true})
	w.cancelRef = ref
	return ref, err
}
func (w *generationFakeExecution) phases() []testgenflow.MavenPhase {
	result := []testgenflow.MavenPhase{}
	for _, call := range w.maven {
		result = append(result, call.Phase)
	}
	return result
}

type generationFakeModel struct {
	files                   artifact.Store
	policy                  string
	status                  map[string]string
	jobs                    map[string]agentworker.StageResult
	calls                   []agentworker.StageRequest
	readyCalls, cancelCalls int
	confirmed               []string
	cleanupCalls            int
	cancelRef               string
	unconfirmedCancellation bool
	sourceEvidence          func(context.Context, string) ([]string, error)
	diagnostic              string
	active                  bool
	unknownAfterComplete    string
}

func (w *generationFakeModel) Ready(context.Context) error { w.readyCalls++; return nil }
func (w *generationFakeModel) PolicyDigest() string        { return w.policy }
func (w *generationFakeModel) RunStage(ctx context.Context, request agentworker.StageRequest) (agentworker.StageResult, error) {
	call := generationClone(request)
	call.Work.ReconcileOnly = request.Work.ReconcileOnly
	call.RecoveryOnly = request.RecoveryOnly
	w.calls = append(w.calls, call)
	if w.sourceEvidence != nil {
		if refs, err := w.sourceEvidence(ctx, request.Work.RunID); err != nil || len(refs) == 0 {
			return agentworker.StageResult{}, &testgenflow.WorkerFailure{Diagnostic: "synthetic generation source discovery unavailable", OutcomeUnknown: true, Cause: err}
		}
	}
	if previous, exists := w.jobs[request.Work.IdempotencyKey]; exists {
		return previous, nil
	}
	if request.Work.ReconcileOnly {
		return agentworker.StageResult{}, &testgenflow.WorkerFailure{Diagnostic: "synthetic prepared model job missing", OutcomeUnknown: true}
	}
	status := w.status[request.Phase]
	if status == "" {
		status = "completed"
	}
	// This retained document is explicitly a synthetic fixture, not a claim to
	// satisfy the real Python worker's bound StageResult protocol.
	diagnostic := w.diagnostic
	if diagnostic == "" {
		diagnostic = "synthetic generation fixture; not live model evidence"
	}
	content := map[string]any{"synthetic": true, "component": "fake_generation_phase", "run_id": request.Work.RunID, "attempt": request.Work.Attempt, "phase": request.Phase, "status": status, "diagnostic": diagnostic, "suggested_test_ids": []string{"example.GeneratedTest#modelSuggested"}}
	if request.Phase == "code" {
		outputRef, err := generationPut(ctx, w.files, map[string]any{"output": map[string]any{"suggested_test_ids": []string{"example.GeneratedTest#modelSuggested"}}})
		if err != nil {
			return agentworker.StageResult{}, err
		}
		content["result_ref"] = outputRef
	}
	ref, err := generationPut(ctx, w.files, content)
	result := agentworker.StageResult{ArtifactRef: ref, Status: status, Diagnostic: diagnostic}
	if err == nil {
		w.jobs[request.Work.IdempotencyKey] = result
		if w.unknownAfterComplete == request.Phase && !request.RecoveryOnly {
			w.unknownAfterComplete = ""
			return agentworker.StageResult{}, &testgenflow.WorkerFailure{Diagnostic: "generation evidence retained but completion acknowledgement unknown", OutcomeUnknown: true}
		}
	}
	return result, err
}
func (w *generationFakeModel) ReadCodeArtifact(ctx context.Context, ref string, ids []string) ([]byte, error) {
	if _, err := w.files.Get(ctx, ref, 32<<20); err != nil {
		return nil, err
	}
	known := false
	for _, request := range w.calls {
		if request.Phase == "code" && w.jobs[request.Work.IdempotencyKey].ArtifactRef == ref {
			known = true
		}
	}
	if !known {
		return nil, errors.New("not a synthetic code phase")
	}
	// Only the explicit human-supplied inventory enters this artifact; the
	// retained phase's modelSuggested ID is deliberately different.
	candidate := localworker.TestArtifact{SchemaVersion: "1", Files: []localworker.TestFile{{Path: generationFixturePath, Content: "package example;\npublic class GeneratedTest { public void humanSelected() {} }\n"}}, ExpectedTestIDs: slices.Clone(ids)}
	raw, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	decoded, err := localworker.DecodeTestArtifact(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}
func (w *generationFakeModel) RunEvidence(context.Context, string) ([]string, error) {
	refs := []string{}
	for _, call := range w.calls {
		refs = append(refs, w.jobs[call.Work.IdempotencyKey].ArtifactRef)
	}
	return refs, nil
}
func (w *generationFakeModel) ConfirmOperations(_ context.Context, _ string, keys []string) error {
	w.confirmed = slices.Clone(keys)
	for _, key := range keys {
		if _, exists := w.jobs[key]; !exists {
			return &testgenflow.WorkerFailure{Diagnostic: "synthetic prepared model job missing; termination unknown", OutcomeUnknown: true}
		}
	}
	return nil
}
func (w *generationFakeModel) CleanupRun(context.Context, string) error { w.cleanupCalls++; return nil }
func (w *generationFakeModel) CancelRun(ctx context.Context, runID string) (string, error) {
	w.cancelCalls++
	if !w.unconfirmedCancellation {
		w.active = false
	}
	ref, err := generationPut(ctx, w.files, map[string]any{"synthetic": true, "component": "fake_generation_cancel", "run_id": runID, "termination_confirmed": !w.unconfirmedCancellation})
	w.cancelRef = ref
	return ref, err
}
func (w *generationFakeModel) phases() []string {
	result := []string{}
	for _, call := range w.calls {
		result = append(result, call.Phase)
	}
	return result
}
