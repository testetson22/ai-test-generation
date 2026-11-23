package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

func TestGenerationReviewCreateUsesServerPolicyAndRequiresAuthentication(t *testing.T) {
	f := newGenerationReviewTestAPI(t)
	for _, token := range []string{"", "incorrect-token"} {
		response := testGenerationRequest(f.handler, http.MethodPost, "/development/test-generation-runs", `{"objective":"Verify the specified behavior"}`, "model-create", token)
		if response.Code != http.StatusUnauthorized || len(f.repository.runs) != 0 || f.approvalCalls != 0 {
			t.Fatalf("unauthenticated model create reached admission: %d %s", response.Code, response.Body.String())
		}
	}
	for _, body := range []string{
		`{}`,
		`{"objective":" "}`,
		`{"objective":"Verify behavior","model":"caller-model"}`,
		`{"objective":"Verify behavior","harness":"caller-harness"}`,
		`{"objective":"Verify behavior","policy_digest":"caller-policy"}`,
		`{"objective":"Verify behavior","context_paths":["unapproved/source"]}`,
		`{"objective":"Verify behavior","input_artifact_digest":"sha256:` + strings.Repeat("a", 64) + `"}`,
	} {
		response := testGenerationRequest(f.handler, http.MethodPost, "/development/test-generation-runs", body, "invalid-model-create", f.api.Config.BearerToken)
		if response.Code != http.StatusBadRequest || len(f.repository.runs) != 0 || f.approvalCalls != 0 {
			t.Fatalf("invalid model create reached admission: %d %s", response.Code, response.Body.String())
		}
	}
	run := f.createRun(t)
	if run.Request.InputMode != testgenflow.InputModeModelGeneration || run.Request.CompletionMode != testgenflow.CompletionModePatchOnly || run.Request.InputArtifactDigest != "" || run.Request.Generation == nil || run.Request.Generation.Objective != "Verify the specified behavior" || run.Request.Generation.PolicyDigest != f.api.Config.GenerationPolicyDigest || !slices.Equal(run.Request.Generation.ContextPaths, []string{"pom.xml"}) || run.Status != testgenflow.StatusQueued || run.AttemptCount() != 0 || f.approvalCalls != 0 {
		t.Fatalf("model creation lost server-selected policy or fabricated work: %#v", run)
	}
}

func TestGenerationReviewIdentityIsServerOwnedAndPlanApprovalResumesRun(t *testing.T) {
	f := newGenerationReviewTestAPI(t)
	run := f.planCheckpoint(t)
	body := generationReviewBody(run, "approve", nil)
	for _, token := range []string{"", "wrong-review-token"} {
		beforeEvents := len(f.repository.events[run.RunID])
		response := testGenerationRequest(f.handler, http.MethodPost, generationReviewPath(run), generationReviewJSON(t, body), "", token)
		if response.Code != http.StatusUnauthorized || len(f.repository.events[run.RunID]) != beforeEvents || f.approvalCalls != 0 {
			t.Fatalf("unauthenticated review changed state: %d %s", response.Code, response.Body.String())
		}
	}
	for _, field := range []string{"reviewer_id", "reviewer_role", "authorization_ref", "attempt", "decided_at", "approved_test_artifact_digest"} {
		t.Run(field, func(t *testing.T) {
			input := generationReviewBody(run, "approve", nil)
			input[field] = "caller-controlled"
			f.assertRejected(t, run, input, http.StatusBadRequest)
		})
	}
	approved := f.review(t, run, body)
	if approved.Status != testgenflow.StatusRunning || approved.Stage != testgenflow.StageGenerate || approved.Checkpoint != nil || approved.Revision != run.Revision+1 || len(approved.Reviews) != 1 || f.approvalCalls != 0 {
		t.Fatalf("plan approval did not resume generation without code retention: %#v", approved)
	}
	review := approved.Reviews[0]
	if review.DecisionID != body["decision_id"] || review.CheckpointID != run.Checkpoint.ID || review.ArtifactRef != run.Checkpoint.ArtifactRef || review.Attempt != 1 || review.Stage != "plan" || review.Action != "approve" || review.ReviewerID != "unit-reviewer" || review.ReviewerRole != "reviewer" || review.AuthorizationRef != "authz:test" || review.Rationale != body["rationale"] || len(review.ExpectedTestIDs) != 0 || review.ApprovedTestArtifactDigest != "" || !review.DecidedAt.Equal(f.now.Truncate(time.Microsecond)) {
		t.Fatalf("plan approval lost scope, attribution, or canonical decision time: %#v", review)
	}
}

func TestGenerationReviewInvalidScopeNeverCallsArtifactRetention(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, test := range []struct {
			name   string
			mutate func(*generationReviewTestAPI, testgenflow.Run, map[string]any)
		}{
			{"wrong artifact", func(_ *generationReviewTestAPI, _ testgenflow.Run, body map[string]any) {
				body["artifact_ref"] = artifact.Digest([]byte("unreviewed artifact"))
			}},
			{"wrong checkpoint", func(_ *generationReviewTestAPI, _ testgenflow.Run, body map[string]any) {
				body["checkpoint_id"] = artifact.Digest([]byte("different checkpoint"))
			}},
			{"wrong stage", func(_ *generationReviewTestAPI, _ testgenflow.Run, body map[string]any) {
				if stage == "plan" {
					body["stage"], body["expected_test_ids"] = "code", []string{"example.GeneratedTest#alpha"}
				} else {
					body["stage"] = "plan"
					delete(body, "expected_test_ids")
				}
			}},
			{"stale revision", func(_ *generationReviewTestAPI, run testgenflow.Run, body map[string]any) {
				body["expected_revision"] = run.Revision - 1
			}},
			{"future revision", func(_ *generationReviewTestAPI, run testgenflow.Run, body map[string]any) {
				body["expected_revision"] = run.Revision + 1
			}},
			{"at expiry", func(f *generationReviewTestAPI, run testgenflow.Run, _ map[string]any) {
				f.now = run.Checkpoint.ExpiresAt
			}},
			{"after expiry", func(f *generationReviewTestAPI, run testgenflow.Run, _ map[string]any) {
				f.now = run.Checkpoint.ExpiresAt.Add(time.Second)
			}},
		} {
			t.Run(stage+"/"+test.name, func(t *testing.T) {
				f := newGenerationReviewTestAPI(t)
				run := f.planCheckpoint(t)
				var ids []string
				if stage == "code" {
					run = f.review(t, run, generationReviewBody(run, "approve", nil))
					run = f.checkpoint(t, run, "code")
					ids = []string{"example.GeneratedTest#alpha"}
				}
				body := generationReviewBody(run, "approve", ids)
				test.mutate(f, run, body)
				f.assertRejected(t, run, body, http.StatusConflict)
			})
		}
	}
}

func TestGenerationCodeReviewValidatesIndependentInventoryAndRationaleBeforeRetention(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing IDs", func(body map[string]any) { delete(body, "expected_test_ids") }},
		{"empty IDs", func(body map[string]any) { body["expected_test_ids"] = []string{} }},
		{"duplicate IDs", func(body map[string]any) {
			body["expected_test_ids"] = []string{"example.GeneratedTest#alpha", "example.GeneratedTest#alpha"}
		}},
		{"wildcard ID", func(body map[string]any) { body["expected_test_ids"] = []string{"example.GeneratedTest#*"} }},
		{"invalid ID", func(body map[string]any) { body["expected_test_ids"] = []string{"not-a-class-method"} }},
		{"missing rationale", func(body map[string]any) { delete(body, "rationale") }},
		{"blank rationale", func(body map[string]any) { body["rationale"] = " " }},
		{"control rationale", func(body map[string]any) { body["rationale"] = "Reviewed\nforged authorization" }},
		{"oversized rationale", func(body map[string]any) { body["rationale"] = strings.Repeat("r", 2049) }},
		{"invalid decision ID", func(body map[string]any) { body["decision_id"] = "invalid decision identity" }},
		{"caller approved digest", func(body map[string]any) {
			body["approved_test_artifact_digest"] = artifact.Digest([]byte("caller-claimed artifact"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newGenerationReviewTestAPI(t)
			run := f.planCheckpoint(t)
			run = f.review(t, run, generationReviewBody(run, "approve", nil))
			run = f.checkpoint(t, run, "code")
			body := generationReviewBody(run, "approve", []string{"example.GeneratedTest#alpha"})
			test.mutate(body)
			f.assertRejected(t, run, body, http.StatusBadRequest)
		})
	}
}

func TestGenerationCodeApprovalRetainsCanonicalArtifactWithHumanInventory(t *testing.T) {
	f := newGenerationReviewTestAPI(t)
	run := f.planCheckpoint(t)
	run = f.review(t, run, generationReviewBody(run, "approve", nil))
	run = f.checkpoint(t, run, "code")
	ids := []string{"example.GeneratedTest#beta", "example.GeneratedTest#alpha"}
	body := generationReviewBody(run, "approve", ids)
	approved := f.review(t, run, body)
	wantIDs := []string{"example.GeneratedTest#alpha", "example.GeneratedTest#beta"}
	if approved.Status != testgenflow.StatusRunning || approved.Checkpoint != nil || approved.Stage != testgenflow.StageGenerate || len(approved.Reviews) != 2 || f.approvalCalls != 1 || !reflect.DeepEqual(f.receivedIDs, [][]string{wantIDs}) || !slices.Equal(f.receivedRefs, []string{run.Checkpoint.ArtifactRef}) {
		t.Fatalf("code approval did not bind independent inventory/current artifact: run=%#v calls=%d ids=%v refs=%v", approved, f.approvalCalls, f.receivedIDs, f.receivedRefs)
	}
	wantBytes := generationReviewArtifact(t, wantIDs)
	wantDigest := artifact.Digest(wantBytes)
	review := approved.Reviews[1]
	if review.ApprovedTestArtifactDigest != wantDigest || !slices.Equal(review.ExpectedTestIDs, wantIDs) || review.ArtifactRef != run.Checkpoint.ArtifactRef || review.CheckpointID != run.Checkpoint.ID || review.ReviewerID != f.api.Config.ReviewerID || review.AuthorizationRef != f.api.Config.ReviewAuthorizationRef {
		t.Fatalf("persisted code approval lost content or human scope: %#v", review)
	}
	retained, err := f.store.Get(t.Context(), wantDigest, 1<<20)
	if err != nil || !bytes.Equal(retained, wantBytes) {
		t.Fatalf("code approval digest is not a retained canonical artifact: %q %v", retained, err)
	}
	response := testGenerationRequest(f.handler, http.MethodGet, "/development/artifacts/"+wantDigest, "", "", f.api.Config.BearerToken)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), wantBytes) {
		t.Fatalf("approved artifact not retrievable: %d %q", response.Code, response.Body.Bytes())
	}
	if approved.TestsAdded != 0 || approved.PatchDigest != "" || approved.PRURL != "" || approved.ResultArtifactRef != "" || approved.Attempts[0].Status != testgenflow.AttemptRunning {
		t.Fatalf("review response fabricated generated/executed/published tests: %#v", approved)
	}
}

func TestGenerationCodeRetentionFailureLeavesCheckpointUndecided(t *testing.T) {
	f := newGenerationReviewTestAPI(t)
	run := f.planCheckpoint(t)
	run = f.review(t, run, generationReviewBody(run, "approve", nil))
	run = f.checkpoint(t, run, "code")
	f.approvalErr = errors.New("injected CAS retention failure")
	body := generationReviewBody(run, "approve", []string{"example.GeneratedTest#alpha"})
	beforeEvents := len(f.repository.events[run.RunID])
	response := testGenerationRequest(f.handler, http.MethodPost, generationReviewPath(run), generationReviewJSON(t, body), "", f.api.Config.BearerToken)
	stored, err := f.repository.GetRun(t.Context(), run.RunID)
	if response.Code != http.StatusBadRequest || err != nil || f.approvalCalls != 1 || !reflect.DeepEqual(stored, run) || len(f.repository.events[run.RunID]) != beforeEvents {
		t.Fatalf("failed CAS retention approved code: status=%d body=%s stored=%#v calls=%d err=%v", response.Code, response.Body.String(), stored, f.approvalCalls, err)
	}
}

func TestGenerationPlanApprovalAndRejectionCannotSupplyTestInventory(t *testing.T) {
	for _, test := range []struct{ stage, action string }{{"plan", "approve"}, {"plan", "reject"}, {"code", "reject"}} {
		t.Run(test.stage+"/"+test.action, func(t *testing.T) {
			f := newGenerationReviewTestAPI(t)
			run := f.planCheckpoint(t)
			if test.stage == "code" {
				run = f.review(t, run, generationReviewBody(run, "approve", nil))
				run = f.checkpoint(t, run, "code")
			}
			f.assertRejected(t, run, generationReviewBody(run, test.action, []string{"example.GeneratedTest#alpha"}), http.StatusBadRequest)
		})
	}
}

func TestGenerationReviewRejectionIsAttributableTerminalAndReplayable(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			f := newGenerationReviewTestAPI(t)
			run := f.planCheckpoint(t)
			if stage == "code" {
				run = f.review(t, run, generationReviewBody(run, "approve", nil))
				run = f.checkpoint(t, run, "code")
			}
			body := generationReviewBody(run, "reject", nil)
			rejected := f.review(t, run, body)
			last := rejected.Reviews[len(rejected.Reviews)-1]
			if rejected.Status != testgenflow.StatusRejected || rejected.Stage != testgenflow.StageGenerate || rejected.Checkpoint != nil || rejected.TestsAdded != 0 || rejected.PRURL != "" || rejected.ResultArtifactRef != "" || last.Action != "reject" || last.ReviewerID != "unit-reviewer" || last.AuthorizationRef != "authz:test" || last.ApprovedTestArtifactDigest != "" || len(last.ExpectedTestIDs) != 0 || f.approvalCalls != 0 || rejected.Attempts[0].Status != testgenflow.AttemptRejected {
				t.Fatalf("rejection invented approval, worker failure, or execution: %#v calls=%d", rejected, f.approvalCalls)
			}
			beforeEvents := len(f.repository.events[run.RunID])
			f.now = f.now.Add(72 * time.Hour)
			replayed := f.review(t, rejected, body)
			if !reflect.DeepEqual(replayed, rejected) || len(f.repository.events[run.RunID]) != beforeEvents || f.approvalCalls != 0 {
				t.Fatalf("terminal rejection replay changed state: %#v", replayed)
			}
			changed := generationReviewBody(run, "reject", nil)
			changed["rationale"] = "Changed rejection rationale"
			f.assertRejected(t, rejected, changed, http.StatusConflict)
		})
	}
}

func TestGenerationReviewReplayPreservesFractionalTimeAcrossLaterPhasesAndCancellation(t *testing.T) {
	f := newGenerationReviewTestAPI(t)
	run := f.planCheckpoint(t)
	planBody := generationReviewBody(run, "approve", nil)
	run = f.review(t, run, planBody)
	planDecision := run.Reviews[0]
	if planDecision.DecidedAt.Nanosecond() != 123456000 {
		t.Fatalf("decision time lost expected microsecond projection: %s", planDecision.DecidedAt)
	}
	f.now = f.now.Add(48 * time.Hour)
	beforeEvents := len(f.repository.events[run.RunID])
	if replay := f.review(t, run, planBody); !reflect.DeepEqual(replay, run) || len(f.repository.events[run.RunID]) != beforeEvents {
		t.Fatalf("plan replay changed timestamp/revision: %#v", replay)
	}
	run = f.checkpoint(t, run, "code")
	beforeEvents = len(f.repository.events[run.RunID])
	if replay := f.review(t, run, planBody); !reflect.DeepEqual(replay, run) || len(f.repository.events[run.RunID]) != beforeEvents || f.approvalCalls != 0 {
		t.Fatalf("plan replay at code checkpoint advanced state: %#v", replay)
	}
	codeBody := generationReviewBody(run, "approve", []string{"example.GeneratedTest#alpha"})
	run = f.review(t, run, codeBody)
	approvedDigest := run.Reviews[1].ApprovedTestArtifactDigest
	// These are synthetic ledger observations, not model or Maven execution.
	run = f.apply(t, run, testgenflow.Event{Type: testgenflow.EventGenerationCompleted, ChangedTestPaths: []string{"src/test/java/example/GeneratedTest.java"}, PatchDigest: artifact.Digest([]byte("synthetic patch observation")), TestArtifactDigest: approvedDigest})
	if run.Stage != testgenflow.StageFocusedTest {
		t.Fatalf("synthetic ledger did not reach later stage: %#v", run)
	}
	f.now = f.now.Add(48 * time.Hour)
	beforeEvents = len(f.repository.events[run.RunID])
	if replay := f.review(t, run, codeBody); !reflect.DeepEqual(replay, run) || len(f.repository.events[run.RunID]) != beforeEvents || f.approvalCalls != 1 {
		t.Fatalf("code replay at later stage repeated artifact retention: %#v calls=%d", replay, f.approvalCalls)
	}
	run = f.apply(t, run, testgenflow.Event{Type: testgenflow.EventCancelRequested})
	run = f.apply(t, run, testgenflow.Event{Type: testgenflow.EventCancelled, TerminationConfirmed: true})
	f.now = f.now.Add(48 * time.Hour)
	beforeEvents = len(f.repository.events[run.RunID])
	if replay := f.review(t, run, codeBody); !reflect.DeepEqual(replay, run) || replay.Status != testgenflow.StatusCancelled || len(f.repository.events[run.RunID]) != beforeEvents || f.approvalCalls != 1 {
		t.Fatalf("code replay after cancellation changed terminal state: %#v calls=%d", replay, f.approvalCalls)
	}
}

func TestGenerationReviewDecisionIdentityBindsPayloadRevisionAndServerActor(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*generationReviewTestAPI, map[string]any)
	}{
		{"rationale", func(_ *generationReviewTestAPI, body map[string]any) {
			body["rationale"] = "A different human rationale"
		}},
		{"action", func(_ *generationReviewTestAPI, body map[string]any) { body["action"] = "reject" }},
		{"artifact", func(_ *generationReviewTestAPI, body map[string]any) {
			body["artifact_ref"] = artifact.Digest([]byte("different artifact"))
		}},
		{"checkpoint", func(_ *generationReviewTestAPI, body map[string]any) {
			body["checkpoint_id"] = artifact.Digest([]byte("different checkpoint"))
		}},
		{"revision", func(_ *generationReviewTestAPI, body map[string]any) {
			body["expected_revision"] = body["expected_revision"].(int64) + 1
		}},
		{"reviewer", func(f *generationReviewTestAPI, _ map[string]any) { f.api.Config.ReviewerID = "other-server-reviewer" }},
		{"authorization", func(f *generationReviewTestAPI, _ map[string]any) {
			f.api.Config.ReviewAuthorizationRef = "authz:different"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newGenerationReviewTestAPI(t)
			run := f.planCheckpoint(t)
			body := generationReviewBody(run, "approve", nil)
			run = f.review(t, run, body)
			f.now = f.now.Add(48 * time.Hour)
			test.mutate(f, body)
			f.assertRejected(t, run, body, http.StatusConflict)
		})
	}
	f := newGenerationReviewTestAPI(t)
	run := f.planCheckpoint(t)
	planBody := generationReviewBody(run, "approve", nil)
	run = f.review(t, run, planBody)
	run = f.checkpoint(t, run, "code")
	codeBody := generationReviewBody(run, "approve", []string{"example.GeneratedTest#alpha"})
	codeBody["decision_id"] = planBody["decision_id"]
	f.assertRejected(t, run, codeBody, http.StatusConflict)
	codeBody = generationReviewBody(run, "approve", []string{"example.GeneratedTest#alpha"})
	run = f.review(t, run, codeBody)
	codeBody["expected_test_ids"] = []string{"example.GeneratedTest#beta"}
	beforeCalls := f.approvalCalls
	f.assertRejected(t, run, codeBody, http.StatusConflict)
	if beforeCalls != 1 || f.approvalCalls != 1 {
		t.Fatalf("altered inventory repeated code retention: calls=%d->%d", beforeCalls, f.approvalCalls)
	}
}

func TestGenerationWaitingReviewCancellationReplaysWithoutGrantingApproval(t *testing.T) {
	f := newGenerationReviewTestAPI(t)
	run := f.planCheckpoint(t)
	path := "/development/test-generation-runs/" + run.RunID + "/cancel"
	body := fmt.Sprintf(`{"expected_revision":%d}`, run.Revision)
	beforeEvents := len(f.repository.events[run.RunID])
	response := testGenerationRequest(f.handler, http.MethodPost, path, body, "cancel-waiting-model", f.api.Config.BearerToken)
	var pending TestGenerationDevelopmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &pending); err != nil || response.Code != http.StatusAccepted || pending.Run.Status != testgenflow.StatusCancelRequested || pending.Run.Checkpoint != nil || pending.Run.CancelRequestedAt == nil || !pending.Run.CancelRequestedAt.Equal(f.now.Truncate(time.Microsecond)) || len(pending.Run.Reviews) != 0 || f.approvalCalls != 0 || len(f.repository.events[run.RunID]) != beforeEvents+1 {
		t.Fatalf("waiting model cancellation fabricated review/termination: %d %s %v", response.Code, response.Body.String(), err)
	}
	f.now = f.now.Add(48 * time.Hour)
	response = testGenerationRequest(f.handler, http.MethodPost, path, body, "cancel-waiting-model", f.api.Config.BearerToken)
	var replayed TestGenerationDevelopmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &replayed); err != nil || response.Code != http.StatusAccepted || !reflect.DeepEqual(replayed.Run, pending.Run) || len(f.repository.events[run.RunID]) != beforeEvents+1 {
		t.Fatalf("model cancel replay changed timestamp/state: %d %s %v", response.Code, response.Body.String(), err)
	}
	terminal := f.apply(t, pending.Run, testgenflow.Event{Type: testgenflow.EventCancelled, TerminationConfirmed: true})
	f.now = f.now.Add(48 * time.Hour)
	response = testGenerationRequest(f.handler, http.MethodPost, path, body, "cancel-waiting-model", f.api.Config.BearerToken)
	if err := json.Unmarshal(response.Body.Bytes(), &replayed); err != nil || response.Code != http.StatusAccepted || !reflect.DeepEqual(replayed.Run, terminal) || len(f.repository.events[run.RunID]) != beforeEvents+2 || f.approvalCalls != 0 {
		t.Fatalf("terminal model cancel replay granted approval: %d %s %v", response.Code, response.Body.String(), err)
	}
}

func TestGenerationReviewAndCancellationRejectInactiveProfilePolicyAndModes(t *testing.T) {
	for _, scope := range []string{"profile", "policy", "artifact run", "legacy run", "artifact server"} {
		t.Run(scope, func(t *testing.T) {
			f := newGenerationReviewTestAPI(t)
			run := f.planCheckpoint(t)
			body := generationReviewBody(run, "reject", nil)
			reviewStatus := http.StatusConflict
			switch scope {
			case "profile":
				f.api.Config.ProfileDigest = artifact.Digest([]byte("new execution profile"))
			case "policy":
				f.api.Config.GenerationPolicyDigest = artifact.Digest([]byte("new generation policy"))
			case "artifact server":
				f.api.Config.InputMode = testgenflow.InputModeArtifactInput
				f.handler = NewTestGenerationDevelopmentHandler(f.api)
				reviewStatus = http.StatusNotFound
			case "artifact run", "legacy run":
				request := run.Request
				request.Generation, request.InputMode = nil, ""
				if scope == "artifact run" {
					request.InputMode = testgenflow.InputModeArtifactInput
					request.InputArtifactDigest = artifact.Digest([]byte(localTestArtifactJSON))
				}
				other, err := testgenflow.NewRun("other-mode-run", "other-mode-key", request, 3, f.now)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := f.repository.CreateRun(t.Context(), other); err != nil {
					t.Fatal(err)
				}
				run = other
				body["expected_revision"] = run.Revision
			}
			f.assertRejected(t, run, body, reviewStatus)
			beforeEvents := len(f.repository.events[run.RunID])
			response := testGenerationRequest(f.handler, http.MethodPost, "/development/test-generation-runs/"+run.RunID+"/cancel", fmt.Sprintf(`{"expected_revision":%d}`, run.Revision), "inactive-cancel", f.api.Config.BearerToken)
			stored, err := f.repository.GetRun(t.Context(), run.RunID)
			if response.Code != http.StatusConflict || err != nil || !reflect.DeepEqual(stored, run) || len(f.repository.events[run.RunID]) != beforeEvents || f.approvalCalls != 0 {
				t.Fatalf("inactive %s cancellation changed state: %d %s stored=%#v err=%v", scope, response.Code, response.Body.String(), stored, err)
			}
		})
	}
}

type generationReviewTestAPI struct {
	api           *TestGenerationDevelopmentAPI
	repository    *memoryTestGenerationRepository
	store         *artifact.FileStore
	handler       http.Handler
	now           time.Time
	approvalCalls int
	receivedRefs  []string
	receivedIDs   [][]string
	approvalErr   error
}

func newGenerationReviewTestAPI(t *testing.T) *generationReviewTestAPI {
	t.Helper()
	api, repository, store := newLocalDevelopmentTestAPI(t)
	f := &generationReviewTestAPI{api: api, repository: repository, store: store, now: api.Config.Now().Add(123456789 * time.Nanosecond)}
	api.Config.InputMode = testgenflow.InputModeModelGeneration
	api.Config.GenerationPolicyDigest = artifact.Digest([]byte("server generation policy"))
	api.Config.GenerationContextPaths = []string{"pom.xml"}
	api.Config.ReviewerID, api.Config.ReviewerRole, api.Config.ReviewAuthorizationRef = "unit-reviewer", "reviewer", "authz:test"
	api.Config.Now = func() time.Time { return f.now }
	api.ApproveGeneratedCode = func(ctx context.Context, ref string, ids []string) (string, error) {
		f.approvalCalls++
		f.receivedRefs = append(f.receivedRefs, ref)
		f.receivedIDs = append(f.receivedIDs, slices.Clone(ids))
		if f.approvalErr != nil {
			return "", f.approvalErr
		}
		retained, err := store.Put(ctx, generationReviewArtifact(t, ids))
		if err != nil {
			return "", err
		}
		return retained.Digest, nil
	}
	f.handler = NewTestGenerationDevelopmentHandler(api)
	if !api.Ready() {
		t.Fatal("model review fixture is unconfigured")
	}
	return f
}

func (f *generationReviewTestAPI) createRun(t *testing.T) testgenflow.Run {
	t.Helper()
	response := testGenerationRequest(f.handler, http.MethodPost, "/development/test-generation-runs", `{"objective":"Verify the specified behavior"}`, "model-objective", f.api.Config.BearerToken)
	var body TestGenerationDevelopmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusCreated {
		t.Fatalf("create model run: %d %s %v", response.Code, response.Body.String(), err)
	}
	return body.Run
}

func (f *generationReviewTestAPI) planCheckpoint(t *testing.T) testgenflow.Run {
	t.Helper()
	run := f.createRun(t)
	for _, event := range []testgenflow.Event{
		{Type: testgenflow.EventSourceResolved, ResolvedCommit: run.Request.Revision},
		{Type: testgenflow.EventBaselineCompleted, Succeeded: true},
		{Type: testgenflow.EventGenerationStarted},
	} {
		run = f.apply(t, run, event)
	}
	return f.checkpoint(t, run, "plan")
}

func (f *generationReviewTestAPI) checkpoint(t *testing.T, run testgenflow.Run, stage string) testgenflow.Run {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"phase":%q,"model_suggested_ids":["model.SuggestedTest#unapproved"]}`, stage))
	ref, err := f.store.Put(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := testgenflow.GenerationCheckpoint{ID: testgenflow.GenerationCheckpointID(run.RunID, run.RequestDigest, run.AttemptCount(), stage, ref.Digest), Stage: stage, ArtifactRef: ref.Digest, Attempt: run.AttemptCount(), ExpiresAt: f.now.Truncate(time.Microsecond).Add(time.Hour)}
	return f.apply(t, run, testgenflow.Event{Type: testgenflow.EventGenerationCheckpointRequested, Checkpoint: &checkpoint})
}

func (f *generationReviewTestAPI) apply(t *testing.T, run testgenflow.Run, event testgenflow.Event) testgenflow.Run {
	t.Helper()
	f.now = f.now.Add(time.Second)
	event.ID, event.ExpectedRevision, event.OccurredAt = fmt.Sprintf("fixture-event-%d", run.Revision), run.Revision, f.now.Truncate(time.Microsecond)
	updated, err := f.repository.ApplyEvent(t.Context(), run.RunID, event)
	if err != nil {
		t.Fatalf("apply synthetic %s: %v", event.Type, err)
	}
	return updated
}

func (f *generationReviewTestAPI) review(t *testing.T, run testgenflow.Run, input map[string]any) testgenflow.Run {
	t.Helper()
	response := testGenerationRequest(f.handler, http.MethodPost, generationReviewPath(run), generationReviewJSON(t, input), "", f.api.Config.BearerToken)
	var body TestGenerationDevelopmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusAccepted {
		t.Fatalf("review: %d %s %v", response.Code, response.Body.String(), err)
	}
	return body.Run
}

func (f *generationReviewTestAPI) assertRejected(t *testing.T, run testgenflow.Run, input map[string]any, status int) {
	t.Helper()
	beforeEvents, beforeCalls := len(f.repository.events[run.RunID]), f.approvalCalls
	response := testGenerationRequest(f.handler, http.MethodPost, generationReviewPath(run), generationReviewJSON(t, input), "", f.api.Config.BearerToken)
	if response.Code != status {
		t.Errorf("review status=%d, want %d: %s", response.Code, status, response.Body.String())
	}
	stored, err := f.repository.GetRun(t.Context(), run.RunID)
	if err != nil || !reflect.DeepEqual(stored, run) || len(f.repository.events[run.RunID]) != beforeEvents || f.approvalCalls != beforeCalls {
		t.Errorf("rejected review changed ledger or called retention: state_equal=%t calls=%d->%d events=%d->%d err=%v", reflect.DeepEqual(stored, run), beforeCalls, f.approvalCalls, beforeEvents, len(f.repository.events[run.RunID]), err)
	}
}

func generationReviewBody(run testgenflow.Run, action string, ids []string) map[string]any {
	c := run.Checkpoint
	body := map[string]any{"expected_revision": run.Revision, "decision_id": "decision-" + c.Stage, "checkpoint_id": c.ID, "stage": c.Stage, "artifact_ref": c.ArtifactRef, "action": action, "rationale": "Human reviewed this exact immutable artifact"}
	if ids != nil {
		body["expected_test_ids"] = ids
	}
	return body
}

func generationReviewPath(run testgenflow.Run) string {
	return "/development/test-generation-runs/" + run.RunID + "/reviews"
}

func generationReviewJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func generationReviewArtifact(t *testing.T, ids []string) []byte {
	t.Helper()
	return []byte(generationReviewJSON(t, map[string]any{
		"schema_version":    "1",
		"files":             []map[string]string{{"path": "src/test/java/example/GeneratedTest.java", "content": "package example; class GeneratedTest { void alpha() {} void beta() {} }\n"}},
		"expected_test_ids": ids,
	}))
}
