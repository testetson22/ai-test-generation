package localservice

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/httpapi"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/store/postgres"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

func TestServiceDriveOncePublishesRetainedPatchOnlyManifest(t *testing.T) {
	h := newServiceHarness(t)
	h.worker.onCleanup = func(ctx context.Context) error {
		manifests := h.artifacts.manifests()
		if len(manifests) != 1 || manifests[0].manifest.CleanupConfirmed {
			return errors.New("cleanup occurred before provisional manifest retention")
		}
		_, err := h.files.Get(ctx, manifests[0].ref, 1<<20)
		return err
	}
	run := h.driveToTerminal(t, h.service)
	if run.Status != testgenflow.StatusCompleted || run.Stage != testgenflow.StageDone || run.TestsAdded != 1 || run.AttemptCount() != 0 || run.PRURL != "" || run.PatchDigest != h.worker.patchDigest || run.TestArtifactDigest != h.worker.artifactDigest || run.ResultArtifactRef == "" {
		t.Fatalf("patch-only completed run = %#v", run)
	}
	pipeline, generation := run.CompatibilityResults()
	if pipeline.ResultArtifactRef != run.ResultArtifactRef || pipeline.PRURL != "" || pipeline.TerminalStatus != "completed" || generation.Attempts != "0" {
		t.Fatalf("patch-only compatibility results = %#v, %#v", pipeline, generation)
	}
	raw := h.get(t, run.ResultArtifactRef)
	var manifest serviceResultManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != "local-execution-result/v1" || manifest.Mode != "artifact_input_patch_only" || manifest.RunID != run.RunID || manifest.Request != run.Request || manifest.ResolvedCommit != run.Request.Revision || manifest.TestArtifactDigest != h.worker.artifactDigest || manifest.PatchDigest != h.worker.patchDigest || !manifest.CleanupConfirmed || !reflect.DeepEqual(manifest.ChangedTestPaths, []string{serviceTestPath}) {
		t.Fatalf("result manifest = %#v", manifest)
	}
	if !bytes.Equal(h.get(t, manifest.PatchDigest), []byte(serviceTestPatch)) || !bytes.Equal(h.get(t, manifest.TestArtifactDigest), h.input) {
		t.Fatal("published manifest does not retrieve the exact retained candidate and unified patch")
	}
	if !reflect.DeepEqual(manifest.WorkerEvidence, []string{h.worker.sourceEvidence}) {
		t.Fatalf("manifest lost source evidence: %v", manifest.WorkerEvidence)
	}
	h.get(t, manifest.WorkerEvidence[0])
	wantPhases := []string{"artifact_input", "maven:focused_tests", "maven:package", "maven:verify", "source"}
	if got := operationKinds(manifest.Operations); !reflect.DeepEqual(got, wantPhases) {
		t.Fatalf("manifest operation kinds = %v, want %v", got, wantPhases)
	}
	for _, op := range manifest.Operations {
		if op.State != "completed" || op.RunID != run.RunID || op.InputDigest == "" {
			t.Fatalf("unbound manifest operation = %#v", op)
		}
		if strings.HasPrefix(op.Kind, "maven:") {
			var result testgenflow.MavenResult
			if err := json.Unmarshal(op.Result, &result); err != nil || !result.Succeeded || result.EvidenceRef == "" {
				t.Fatalf("Maven operation result = %#v, err=%v", result, err)
			}
			h.verifyEvidence(t, result.EvidenceRef, strings.TrimPrefix(op.Kind, "maven:"), true)
		}
	}
	ops := h.operations(t)
	wantJournal := append(append([]string{}, wantPhases...), "publication_evidence", "publish_patch")
	slices.Sort(wantJournal)
	if got := operationKinds(ops); !reflect.DeepEqual(got, wantJournal) {
		t.Fatalf("journal kinds = %v, want %v", got, wantJournal)
	}
	for _, op := range ops {
		if op.State != "completed" {
			t.Fatalf("completed flow has unresolved journal entry: %#v", op)
		}
	}
	calls, launches, cleanups, cancels := h.worker.snapshot()
	if got := workerCallKinds(calls); !reflect.DeepEqual(got, []string{"source", "maven:package", "artifact_input", "maven:focused_tests", "maven:verify"}) || launches != 1 || cleanups != 1 || cancels != 0 {
		t.Fatalf("worker calls=%v source launches=%d cleanup=%d cancellation=%d", got, launches, cleanups, cancels)
	}
	for _, call := range calls[3:] {
		if call.work.TestArtifactDigest != h.worker.artifactDigest || call.work.PatchDigest != h.worker.patchDigest || !reflect.DeepEqual(call.work.ChangedTestPaths, []string{serviceTestPath}) {
			t.Fatalf("focused/verify work is not bound to the retained candidate: %#v", call)
		}
	}
	manifests := h.artifacts.manifests()
	if len(manifests) != 2 || manifests[0].manifest.CleanupConfirmed || !manifests[1].manifest.CleanupConfirmed || manifests[1].ref != run.ResultArtifactRef {
		t.Fatalf("provisional/final manifest sequence = %#v", manifests)
	}
	var provisional string
	publication := findServiceOperation(t, ops, "publication_evidence")
	if err := json.Unmarshal(publication.Result, &provisional); err != nil || provisional != manifests[0].ref || manifest.ProvisionalRef != provisional {
		t.Fatalf("final manifest lost durable pre-cleanup reference: op=%#v manifest=%#v err=%v", publication, manifest, err)
	}
}

func TestServiceSourceResultLostAcknowledgementReplaysWithoutWorkerCall(t *testing.T) {
	h := newServiceHarness(t)
	repository := &faultServiceRepository{Repository: h.store}
	repository.loseSourceAcknowledgement.Store(true)
	service := h.newService(t, repository)
	worked, err := service.DriveOnce(t.Context())
	if err != nil || worked {
		t.Fatalf("lost acknowledgement drive worked=%t err=%v; want pending", worked, err)
	}
	if run := h.run(t); run.Revision != 1 || run.Stage != testgenflow.StageSource {
		t.Fatalf("unacknowledged result prematurely projected: %#v", run)
	}
	op := findServiceOperation(t, h.operations(t), "source")
	var commit string
	if err := json.Unmarshal(op.Result, &commit); err != nil || op.State != "completed" || commit != h.initial.Request.Revision {
		t.Fatalf("lost acknowledgement did not retain completed result: %#v, err=%v", op, err)
	}
	restarted := h.newService(t, h.store)
	h.drive(t, restarted)
	if run := h.run(t); run.Revision != 2 || run.Stage != testgenflow.StageBaseline {
		t.Fatalf("replayed source result = %#v", run)
	}
	calls, launches, _, _ := h.worker.snapshot()
	if len(calls) != 1 || calls[0].kind != "source" || calls[0].work.ReconcileOnly || launches != 1 {
		t.Fatalf("lost acknowledgement reinvoked worker: calls=%#v launches=%d", calls, launches)
	}
	if got := findServiceOperation(t, h.operations(t), "source"); !reflect.DeepEqual(got, op) {
		t.Fatalf("completed replay changed journal outcome: %#v, want %#v", got, op)
	}
}

func TestServiceCancellationRetentionFailureRecoversAfterRestart(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprint(external), func(t *testing.T) {
			h := newServiceHarness(t)
			if external {
				h.drive(t, h.service)
				h.artifacts.failCancelGet.Store(true)
			} else {
				h.artifacts.failCancelPut.Store(true)
			}
			run := h.run(t)
			if _, err := h.store.ApplyEvent(t.Context(), run.RunID, testgenflow.Event{ID: "cancel-retention-test", ExpectedRevision: run.Revision, Type: testgenflow.EventCancelRequested, OccurredAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if worked, err := h.service.DriveOnce(t.Context()); err != nil || worked {
				t.Fatalf("transient cancellation retention: worked=%t err=%v", worked, err)
			}
			if current := h.run(t); current.Status != testgenflow.StatusCancelRequested {
				t.Fatalf("retention failure finalized cancellation: %#v", current)
			}
			if op := findServiceOperation(t, h.operations(t), "cancel"); op.State != "prepared" {
				t.Fatalf("transient retention became permanent: %#v", op)
			}
			_, launches, cleanups, _ := h.worker.snapshot()
			if cleanups != 0 {
				t.Fatal("cleanup occurred before cancellation evidence retention")
			}
			restarted := h.newService(t, h.store)
			h.drive(t, restarted)
			settled := h.run(t)
			if settled.Status != testgenflow.StatusCancelled || settled.EvidenceRef == "" {
				t.Fatalf("cancellation did not recover: %#v", settled)
			}
			h.get(t, settled.EvidenceRef)
			if op := findServiceOperation(t, h.operations(t), "cancel"); op.State != "completed" {
				t.Fatalf("cancellation not acknowledged: %#v", op)
			}
			_, afterLaunches, afterCleanup, _ := h.worker.snapshot()
			if launches != afterLaunches || afterCleanup != 1 {
				t.Fatalf("restart launched work or lost cleanup: launches=%d/%d cleanup=%d", launches, afterLaunches, afterCleanup)
			}
		})
	}
}

func TestServicePreviouslyFailedCancellationSettlesAsUnknown(t *testing.T) {
	h := newServiceHarness(t)
	if _, err := h.store.ApplyEvent(t.Context(), h.initial.RunID, testgenflow.Event{ID: "cancel-legacy-failed", ExpectedRevision: 1, Type: testgenflow.EventCancelRequested, OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	claim, err := h.store.ClaimNextTestGenerationRun(t.Context(), "old-cancel-driver", h.api.ProfileDigest, time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim: %#v %v", claim, err)
	}
	input, _ := json.Marshal(struct {
		RunID         string
		RequestDigest string
	}{h.initial.RunID, h.initial.RequestDigest})
	key := h.initial.RunID + "/cancel"
	if _, created, err := h.store.BeginTestGenerationOperation(t.Context(), *claim, key, "cancel", artifact.Digest(input)); err != nil || !created {
		t.Fatalf("seed cancel: %t %v", created, err)
	}
	if err := h.store.FinishTestGenerationOperation(t.Context(), *claim, key, "failed", nil, "prior cancellation retention failure"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.ReleaseTestGenerationClaim(t.Context(), *claim); err != nil {
		t.Fatal(err)
	}
	h.drive(t, h.service)
	if run := h.run(t); run.Status != testgenflow.StatusOutcomeUnknown {
		t.Fatalf("failed cancellation remains stuck or claimed success: %#v", run)
	}
	if worked, err := h.service.DriveOnce(t.Context()); worked || err != nil {
		t.Fatalf("unknown cancellation redispatched: %t %v", worked, err)
	}
	calls, launches, cleanups, cancels := h.worker.snapshot()
	if len(calls) != 0 || launches != 0 || cleanups != 0 || cancels != 0 {
		t.Fatalf("legacy cancellation invoked work: %v %d %d %d", calls, launches, cleanups, cancels)
	}
}

func TestServiceCancelledPreparedOperationRestartsAsSameJobReconciliation(t *testing.T) {
	h := newServiceHarness(t)
	h.worker.blockSource.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := h.service.DriveOnce(ctx); done <- err }()
	waitServiceStart(t, h.worker.sourceStarted, done)
	cancel()
	if err := waitServiceResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted DriveOnce = %v", err)
	}
	op := findServiceOperation(t, h.operations(t), "source")
	if op.State != "prepared" || op.Result != nil || h.run(t).Revision != 1 {
		t.Fatalf("cancelled callback projected a result: op=%#v run=%#v", op, h.run(t))
	}
	h.worker.blockSource.Store(false)
	restarted := h.newService(t, h.store)
	h.drive(t, restarted)
	calls, launches, cleanups, _ := h.worker.snapshot()
	if len(calls) != 2 || calls[0].work.ReconcileOnly || !calls[1].work.ReconcileOnly || calls[0].work.IdempotencyKey != calls[1].work.IdempotencyKey || launches != 1 || cleanups != 0 {
		t.Fatalf("restart did not reconcile the same external job: calls=%#v launches=%d cleanup=%d", calls, launches, cleanups)
	}
	if run := h.run(t); run.Stage != testgenflow.StageBaseline || run.Revision != 2 {
		t.Fatalf("reconciled source run = %#v", run)
	}
}

func TestServiceMissingPreparedWorkerBecomesUnknownWithoutRedispatch(t *testing.T) {
	h := newServiceHarness(t)
	h.worker.pendingSource.Store(true)
	if worked, err := h.service.DriveOnce(t.Context()); err != nil || worked {
		t.Fatalf("pending source worked=%t err=%v", worked, err)
	}
	h.worker.missingJob.Store(true)
	restarted := h.newService(t, h.store)
	h.drive(t, restarted)
	run := h.run(t)
	if run.Status != testgenflow.StatusOutcomeUnknown || run.Revision != 2 || run.ResultArtifactRef != "" {
		t.Fatalf("missing worker outcome = %#v", run)
	}
	if op := findServiceOperation(t, h.operations(t), "source"); op.State != "unknown" || op.Diagnostic != "prepared worker is missing" {
		t.Fatalf("unknown worker journal = %#v", op)
	}
	if worked, err := restarted.DriveOnce(t.Context()); err != nil || worked {
		t.Fatalf("unknown run was redispatched worked=%t err=%v", worked, err)
	}
	calls, launches, cleanups, _ := h.worker.snapshot()
	if len(calls) != 2 || calls[0].work.ReconcileOnly || !calls[1].work.ReconcileOnly || launches != 1 || cleanups != 0 {
		t.Fatalf("unknown job created extra external work: calls=%#v launches=%d cleanup=%d", calls, launches, cleanups)
	}
}

func TestServiceCancellationHeartbeatRetainsTerminationEvidenceBeforeCleanup(t *testing.T) {
	h := newServiceHarness(t)
	h.worker.blockSource.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := h.service.DriveOnce(ctx); done <- err }()
	waitServiceStart(t, h.worker.sourceStarted, done)
	_, err := h.store.ApplyEvent(t.Context(), h.initial.RunID, testgenflow.Event{
		ID: "request-cancel", ExpectedRevision: 1, Type: testgenflow.EventCancelRequested, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitServiceResult(t, done); err != nil {
		t.Fatalf("heartbeat interruption = %v", err)
	}
	waitServiceSignal(t, h.worker.sourceInterrupted)
	if run := h.run(t); run.Status != testgenflow.StatusCancelRequested || run.Revision != 2 {
		t.Fatalf("heartbeat falsely settled cancellation: %#v", run)
	}
	h.worker.onCleanup = func(ctx context.Context) error {
		ops, err := h.store.ListTestGenerationOperations(ctx, h.initial.RunID)
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.Kind == "cancel" && op.State == "completed" {
				var ref string
				if err := json.Unmarshal(op.Result, &ref); err != nil {
					return err
				}
				_, err := h.files.Get(ctx, ref, 1<<20)
				return err
			}
		}
		return errors.New("cleanup before cancellation manifest was journaled")
	}
	h.drive(t, h.service)
	run := h.run(t)
	if run.Status != testgenflow.StatusCancelled || run.Revision != 3 || run.EvidenceRef == "" || run.ResultArtifactRef != "" {
		t.Fatalf("confirmed cancellation = %#v", run)
	}
	var manifest struct {
		RunID                string   `json:"run_id"`
		TerminationConfirmed bool     `json:"termination_confirmed"`
		EvidenceRefs         []string `json:"evidence_refs"`
	}
	if err := json.Unmarshal(h.get(t, run.EvidenceRef), &manifest); err != nil || manifest.RunID != run.RunID || !manifest.TerminationConfirmed || len(manifest.EvidenceRefs) != 1 {
		t.Fatalf("retained cancellation manifest = %#v, err=%v", manifest, err)
	}
	h.get(t, manifest.EvidenceRefs[0])
	calls, launches, cleanups, cancels := h.worker.snapshot()
	if len(calls) != 1 || launches != 1 || cleanups != 1 || cancels != 1 {
		t.Fatalf("cancellation calls=%#v launches=%d cleanup=%d cancel=%d", calls, launches, cleanups, cancels)
	}
}

func TestServiceFailedMavenRetainsEvidenceBeforeCleanup(t *testing.T) {
	for _, phase := range []testgenflow.MavenPhase{testgenflow.MavenPhasePackage, testgenflow.MavenPhaseFocusedTests, testgenflow.MavenPhaseFinalVerify} {
		t.Run(string(phase), func(t *testing.T) {
			h := newServiceHarness(t)
			h.worker.failedPhase = phase
			cleanupObserved := false
			h.worker.onCleanup = func(ctx context.Context) error {
				ops, err := h.store.ListTestGenerationOperations(ctx, h.initial.RunID)
				if err != nil {
					return err
				}
				for _, op := range ops {
					if op.Kind != "maven:"+string(phase) {
						continue
					}
					var result testgenflow.MavenResult
					if err := json.Unmarshal(op.Result, &result); err != nil || op.State != "completed" || result.Succeeded || result.EvidenceRef == "" {
						return errors.New("failed Maven observation was not journaled before cleanup")
					}
					_, err := h.files.Get(ctx, result.EvidenceRef, 1<<20)
					cleanupObserved = err == nil
					return err
				}
				return errors.New("cleanup before failed Maven evidence")
			}
			run := h.driveToTerminal(t, h.service)
			if run.Status != testgenflow.StatusFailed || run.ResultArtifactRef != "" || run.PRURL != "" || !cleanupObserved {
				t.Fatalf("failed Maven flow = %#v, evidence before cleanup=%t", run, cleanupObserved)
			}
			op := findServiceOperation(t, h.operations(t), "maven:"+string(phase))
			var result testgenflow.MavenResult
			if err := json.Unmarshal(op.Result, &result); err != nil || result.Succeeded || result.Diagnostic != "fake Maven assertion failed" {
				t.Fatalf("retained failed result = %#v, err=%v", result, err)
			}
			h.verifyEvidence(t, result.EvidenceRef, string(phase), false)
			if phase == testgenflow.MavenPhaseFocusedTests {
				if run.Candidate == nil || run.Candidate.FocusedTestEvidenceRef != result.EvidenceRef {
					t.Fatalf("failed candidate lost focused evidence: %#v", run.Candidate)
				}
			} else if run.EvidenceRef != result.EvidenceRef {
				t.Fatalf("failed run evidence = %q, want %q", run.EvidenceRef, result.EvidenceRef)
			}
			_, _, cleanups, _ := h.worker.snapshot()
			if cleanups != 1 || len(h.artifacts.manifests()) != 0 {
				t.Fatalf("failed phase cleanup=%d published=%d", cleanups, len(h.artifacts.manifests()))
			}
		})
	}
}

func TestServiceCancellationBeforeDispatchRetainsNoDispatchManifest(t *testing.T) {
	h := newServiceHarness(t)
	if _, err := h.store.ApplyEvent(t.Context(), h.initial.RunID, testgenflow.Event{ID: "cancel-before-dispatch", ExpectedRevision: 1, Type: testgenflow.EventCancelRequested, OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	h.drive(t, h.service)
	run := h.run(t)
	if run.Status != testgenflow.StatusCancelled || run.EvidenceRef == "" || run.Revision != 3 {
		t.Fatalf("undispatched cancellation = %#v", run)
	}
	var manifest struct {
		RunID                        string `json:"run_id"`
		TerminationConfirmed         bool   `json:"termination_confirmed"`
		ExternalOperationsDispatched *bool  `json:"external_operations_dispatched"`
	}
	if err := json.Unmarshal(h.get(t, run.EvidenceRef), &manifest); err != nil || manifest.RunID != run.RunID || !manifest.TerminationConfirmed || manifest.ExternalOperationsDispatched == nil || *manifest.ExternalOperationsDispatched {
		t.Fatalf("no-dispatch cancellation manifest = %#v, err=%v", manifest, err)
	}
	calls, launches, cleanups, cancels := h.worker.snapshot()
	if len(calls) != 0 || launches != 0 || cleanups != 1 || cancels != 0 {
		t.Fatalf("queued cancellation invoked worker: calls=%#v launches=%d cleanup=%d cancel=%d", calls, launches, cleanups, cancels)
	}
	if kinds := operationKinds(h.operations(t)); !reflect.DeepEqual(kinds, []string{"cancel"}) {
		t.Fatalf("queued cancellation created external intents: %v", kinds)
	}
}

func TestServiceCancellationOfMissingPreparedJobRemainsUnknown(t *testing.T) {
	h := newServiceHarness(t)
	h.worker.pendingSource.Store(true)
	if worked, err := h.service.DriveOnce(t.Context()); err != nil || worked {
		t.Fatalf("pending source worked=%t err=%v", worked, err)
	}
	if _, err := h.store.ApplyEvent(t.Context(), h.initial.RunID, testgenflow.Event{ID: "cancel-missing-worker", ExpectedRevision: 1, Type: testgenflow.EventCancelRequested, OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	h.worker.missingJob.Store(true)
	h.drive(t, h.service)
	run := h.run(t)
	if run.Status != testgenflow.StatusOutcomeUnknown || run.Revision != 3 || run.EvidenceRef != "" || run.ResultArtifactRef != "" {
		t.Fatalf("missing prepared job falsely confirmed cancellation: %#v", run)
	}
	if op := findServiceOperation(t, h.operations(t), "cancel"); op.State != "unknown" {
		t.Fatalf("ambiguous cancellation journal = %#v", op)
	}
	calls, launches, cleanups, cancels := h.worker.snapshot()
	if len(calls) != 1 || launches != 1 || cleanups != 0 || cancels != 0 {
		t.Fatalf("ambiguous cancellation side effects: calls=%#v launches=%d cleanup=%d cancel=%d", calls, launches, cleanups, cancels)
	}
}

func TestServicePublisherRetentionFailurePreventsCleanupAndCompletion(t *testing.T) {
	h := newServiceHarness(t)
	h.artifacts.failManifest.Store(true)
	run := h.driveToTerminal(t, h.service)
	_, _, cleanups, _ := h.worker.snapshot()
	if run.Status != testgenflow.StatusFailed || run.ResultArtifactRef != "" || cleanups != 0 || len(h.artifacts.manifests()) != 0 {
		t.Fatalf("publication retention failure run=%#v cleanup=%d retained manifests=%d", run, cleanups, len(h.artifacts.manifests()))
	}
	if op := findServiceOperation(t, h.operations(t), "publish_patch"); op.State != "failed" {
		t.Fatalf("failed publication journal = %#v", op)
	}
}

func TestServiceUnconfirmedCleanupCannotPublishCompletedResult(t *testing.T) {
	h := newServiceHarness(t)
	h.worker.cleanupError = errors.New("injected unconfirmed cleanup")
	run := h.driveToTerminal(t, h.service)
	_, _, cleanups, _ := h.worker.snapshot()
	manifests := h.artifacts.manifests()
	if run.Status != testgenflow.StatusOutcomeUnknown || run.ResultArtifactRef != "" || cleanups != 1 || len(manifests) != 1 || manifests[0].manifest.CleanupConfirmed {
		t.Fatalf("unconfirmed cleanup run=%#v cleanup=%d manifests=%#v", run, cleanups, manifests)
	}
	h.get(t, manifests[0].ref)
	op := findServiceOperation(t, h.operations(t), "publish_patch")
	var retained struct {
		EvidenceRef string `json:"evidence_ref"`
	}
	if err := json.Unmarshal(op.Result, &retained); err != nil || op.State != "unknown" || retained.EvidenceRef != manifests[0].ref {
		t.Fatalf("unconfirmed publication journal = %#v", op)
	}
}

func TestServicePublicationRestartAfterCleanupUsesJournaledEvidence(t *testing.T) {
	h := newServiceHarness(t)
	h.worker.evidenceUnavailableAfterCleanup.Store(true)
	for range 5 {
		h.drive(t, h.service)
	}
	before := h.run(t)
	if before.Stage != testgenflow.StagePublishPatch || before.Revision != 6 {
		t.Fatalf("publication prerequisites = %#v", before)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.artifacts.cancelFinal = cancel
	h.artifacts.cancelFinalPut.Store(true)
	if worked, err := h.service.DriveOnce(ctx); !worked || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted final publication worked=%t err=%v", worked, err)
	}
	if run := h.run(t); !reflect.DeepEqual(run, before) {
		t.Fatalf("interrupted publication advanced lifecycle = %#v", run)
	}
	ops := h.operations(t)
	evidence := findServiceOperation(t, ops, "publication_evidence")
	outer := findServiceOperation(t, ops, "publish_patch")
	var provisional string
	if err := json.Unmarshal(evidence.Result, &provisional); err != nil || evidence.State != "completed" || provisional == "" || outer.State != "prepared" {
		t.Fatalf("crash recovery journal evidence=%#v publication=%#v err=%v", evidence, outer, err)
	}
	h.get(t, provisional)
	callsBefore, _, cleanups, _ := h.worker.snapshot()
	if cleanups != 1 || len(h.artifacts.manifests()) != 1 {
		t.Fatalf("crash seam did not follow cleanup: cleanup=%d retained manifests=%d", cleanups, len(h.artifacts.manifests()))
	}
	restarted := h.newService(t, h.store)
	h.drive(t, restarted)
	run := h.run(t)
	if run.Status != testgenflow.StatusCompleted || run.ResultArtifactRef == "" {
		t.Fatalf("publication restart failed to use retained evidence: %#v", run)
	}
	var final serviceResultManifest
	if err := json.Unmarshal(h.get(t, run.ResultArtifactRef), &final); err != nil || !final.CleanupConfirmed || final.ProvisionalRef != provisional {
		t.Fatalf("recovered final manifest = %#v, err=%v", final, err)
	}
	callsAfter, launches, cleanups, _ := h.worker.snapshot()
	if !reflect.DeepEqual(callsBefore, callsAfter) || launches != 1 || cleanups != 2 || len(h.artifacts.manifests()) != 2 {
		t.Fatalf("publication restart repeated external work: before=%#v after=%#v launches=%d cleanup=%d", callsBefore, callsAfter, launches, cleanups)
	}
}

func TestServiceReadinessRequiresRunningDriverAndHealthyDependencies(t *testing.T) {
	h := newServiceHarness(t)
	repository := &faultServiceRepository{Repository: h.store}
	service := h.newService(t, repository)
	handler := service.Handler()
	assertReady := func(want int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
		var body struct {
			Status          string `json:"status"`
			Mode            string `json:"mode"`
			LiveGeneration  bool   `json:"live_generation"`
			ProductionReady bool   `json:"production_ready"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != want || body.Mode != "local-development" || body.LiveGeneration || body.ProductionReady {
			t.Fatalf("readiness response = %d %s, err=%v", response.Code, response.Body.String(), err)
		}
		wantState := "unavailable"
		if want == http.StatusOK {
			wantState = "ready"
		}
		if body.Status != wantState {
			t.Fatalf("readiness state=%q, want %q", body.Status, wantState)
		}
	}
	assertReady(http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if err := waitServiceResult(t, done); !errors.Is(err, context.Canceled) {
				t.Errorf("stopped Run = %v, want context cancellation", err)
			}
		})
	}
	t.Cleanup(stop)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !service.running.Load() {
		select {
		case <-deadline.C:
			t.Fatal("driver did not start")
		case <-ticker.C:
		}
	}
	assertReady(http.StatusOK)
	repository.failPing.Store(true)
	assertReady(http.StatusServiceUnavailable)
	repository.failPing.Store(false)
	h.worker.failReady.Store(true)
	assertReady(http.StatusServiceUnavailable)
	h.worker.failReady.Store(false)
	h.artifacts.failReadiness.Store(true)
	assertReady(http.StatusServiceUnavailable)
	h.artifacts.failReadiness.Store(false)
	assertReady(http.StatusOK)
	stop()
	assertReady(http.StatusServiceUnavailable)
}

const serviceTestPath = "src/test/java/example/AddedTest.java"

const serviceTestJava = "package example;\n\nclass AddedTest {\n    @org.junit.jupiter.api.Test\n    void verifies() { org.junit.jupiter.api.Assertions.assertEquals(4, 2 + 2); }\n}\n"

const serviceTestPatch = "diff --git a/src/test/java/example/AddedTest.java b/src/test/java/example/AddedTest.java\nnew file mode 100644\n--- /dev/null\n+++ b/src/test/java/example/AddedTest.java\n@@ -0,0 +1,6 @@\n+package example;\n+\n+class AddedTest {\n+    @org.junit.jupiter.api.Test\n+    void verifies() { org.junit.jupiter.api.Assertions.assertEquals(4, 2 + 2); }\n+}\n"

type serviceHarness struct {
	store     *postgres.Store
	files     *artifact.FileStore
	artifacts *faultServiceArtifacts
	worker    *fakeServiceWorker
	service   *Service
	initial   testgenflow.Run
	input     []byte
	api       httpapi.TestGenerationDevelopmentConfig
}

func newServiceHarness(t *testing.T) *serviceHarness {
	t.Helper()
	h := &serviceHarness{store: isolatedServiceStore(t)}
	files, err := artifact.NewFileStore(t.TempDir(), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	h.files = files
	h.artifacts = &faultServiceArtifacts{Store: files}
	h.input, err = json.Marshal(struct {
		SchemaVersion string              `json:"schema_version"`
		Files         []map[string]string `json:"files"`
		ExpectedIDs   []string            `json:"expected_test_ids"`
	}{"1", []map[string]string{{"path": serviceTestPath, "content": serviceTestJava}}, []string{"example.AddedTest#verifies"}})
	if err != nil {
		t.Fatal(err)
	}
	inputRef, err := files.Put(t.Context(), h.input)
	if err != nil {
		t.Fatal(err)
	}
	patchRef, err := files.Put(t.Context(), []byte(serviceTestPatch))
	if err != nil {
		t.Fatal(err)
	}
	sourceRef, err := files.Put(t.Context(), []byte(`{"schema_version":"fake-source-evidence/v1","revision":"`+strings.Repeat("a", 40)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	h.worker = &fakeServiceWorker{
		store: files, artifactDigest: inputRef.Digest, patchDigest: patchRef.Digest,
		sourceEvidence: sourceRef.Digest,
		jobs:           map[string]bool{}, sourceStarted: make(chan struct{}, 1), sourceInterrupted: make(chan struct{}, 1),
	}
	h.api = httpapi.TestGenerationDevelopmentConfig{
		BearerToken: "local-service-test-bearer-token-0001", RepositoryURL: "https://git.example/group/spring.git", PinnedRevision: strings.Repeat("a", 40),
		DefaultBaseBranch: "main", Subdirectory: ".", ProfileID: "fake-java17-profile", ProfileVersion: "1.0.0", ProfileDigest: artifact.Digest([]byte("fake isolated profile")),
		MaxAttempts: 1, InputMode: testgenflow.InputModeArtifactInput, CompletionMode: testgenflow.CompletionModePatchOnly,
	}
	request := testgenflow.Request{
		RepositoryURL: h.api.RepositoryURL, Revision: h.api.PinnedRevision, BaseBranch: "main", Subdirectory: ".",
		ProfileID: h.api.ProfileID, ProfileVersion: h.api.ProfileVersion, ProfileDigest: h.api.ProfileDigest,
		InputMode: h.api.InputMode, InputArtifactDigest: inputRef.Digest, CompletionMode: h.api.CompletionMode,
	}
	run, err := testgenflow.NewRun("service-test-run", "service-test-idempotency", request, 1, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var inserted bool
	h.initial, inserted, err = h.store.CreateRun(t.Context(), run)
	if err != nil || !inserted {
		t.Fatalf("create isolated service run inserted=%t err=%v", inserted, err)
	}
	h.service = h.newService(t, h.store)
	return h
}

func isolatedServiceStore(t *testing.T) *postgres.Store {
	t.Helper()
	databaseURL := os.Getenv("ATG_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("ATG_TEST_DATABASE_URL is not configured; local-service PostgreSQL tests skipped")
	}
	admin, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "local_service_" + hex.EncodeToString(random[:])
	if _, err := admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop isolated service schema: %v", err)
		}
		_ = admin.Close()
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := postgres.Open(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func (h *serviceHarness) newService(t *testing.T, repository Repository) *Service {
	t.Helper()
	service, err := New(Config{
		Repository: repository, Worker: h.worker, Artifacts: h.artifacts, API: h.api,
		ValidateTestArtifact: func(raw []byte) error {
			if !bytes.Equal(raw, h.input) {
				return errors.New("unexpected test artifact")
			}
			return nil
		}, MaxTestArtifactBytes: 1 << 20,
		PollInterval: 10 * time.Millisecond, LeaseDuration: 400 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func (h *serviceHarness) drive(t *testing.T, service *Service) {
	t.Helper()
	if worked, err := service.DriveOnce(t.Context()); err != nil || !worked {
		t.Fatalf("DriveOnce worked=%t err=%v", worked, err)
	}
}

func (h *serviceHarness) driveToTerminal(t *testing.T, service *Service) testgenflow.Run {
	t.Helper()
	for range 10 {
		run := h.run(t)
		if run.Status == testgenflow.StatusCompleted || run.Status == testgenflow.StatusFailed || run.Status == testgenflow.StatusCancelled || run.Status == testgenflow.StatusOutcomeUnknown {
			return run
		}
		h.drive(t, service)
	}
	t.Fatalf("flow did not settle: %#v", h.run(t))
	return testgenflow.Run{}
}

func (h *serviceHarness) run(t *testing.T) testgenflow.Run {
	t.Helper()
	run, err := h.store.GetRun(t.Context(), h.initial.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (h *serviceHarness) operations(t *testing.T) []testgenflow.Operation {
	t.Helper()
	ops, err := h.store.ListTestGenerationOperations(t.Context(), h.initial.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func (h *serviceHarness) get(t *testing.T, digest string) []byte {
	t.Helper()
	content, err := h.files.Get(t.Context(), digest, 1<<20)
	if err != nil || artifact.Digest(content) != digest {
		t.Fatalf("retained artifact %s: %v", digest, err)
	}
	return content
}

func (h *serviceHarness) verifyEvidence(t *testing.T, ref, phase string, succeeded bool) {
	t.Helper()
	var manifest struct {
		Phase        string `json:"phase"`
		RunID        string `json:"run_id"`
		Succeeded    bool   `json:"succeeded"`
		ReportDigest string `json:"report_digest"`
	}
	if err := json.Unmarshal(h.get(t, ref), &manifest); err != nil || manifest.Phase != phase || manifest.RunID != h.initial.RunID || manifest.Succeeded != succeeded || manifest.ReportDigest == "" {
		t.Fatalf("worker evidence = %#v, err=%v", manifest, err)
	}
	report := h.get(t, manifest.ReportDigest)
	testCase := `<testcase classname="example.AddedTest" name="verifies">`
	if phase == "package" {
		testCase = `<testcase classname="example.BaselineTest" name="baseline">`
	}
	if !bytes.Contains(report, []byte(testCase)) || bytes.Contains(report, []byte("<failure>")) == succeeded {
		t.Fatalf("retained fake report contradicts phase result: %s", report)
	}
}

type serviceResultManifest struct {
	SchemaVersion      string                  `json:"schema_version"`
	Mode               string                  `json:"mode"`
	RunID              string                  `json:"run_id"`
	Request            testgenflow.Request     `json:"request"`
	ResolvedCommit     string                  `json:"resolved_commit"`
	TestArtifactDigest string                  `json:"test_artifact_digest"`
	PatchDigest        string                  `json:"patch_digest"`
	ChangedTestPaths   []string                `json:"changed_test_paths"`
	Operations         []testgenflow.Operation `json:"operations"`
	WorkerEvidence     []string                `json:"worker_evidence"`
	CleanupConfirmed   bool                    `json:"cleanup_confirmed"`
	ProvisionalRef     string                  `json:"provisional_ref"`
}

type retainedServiceManifest struct {
	manifest serviceResultManifest
	ref      string
}

type faultServiceArtifacts struct {
	artifact.Store
	failManifest   atomic.Bool
	failReadiness  atomic.Bool
	failCancelPut  atomic.Bool
	failCancelGet  atomic.Bool
	cancelFinalPut atomic.Bool
	cancelFinal    context.CancelFunc
	mu             sync.Mutex
	retained       []retainedServiceManifest
}

func (s *faultServiceArtifacts) Put(ctx context.Context, content []byte) (artifact.Ref, error) {
	if bytes.Contains(content, []byte(`"schema_version":"local-cancellation/v1"`)) && s.failCancelPut.CompareAndSwap(true, false) {
		return artifact.Ref{}, errors.New("injected transient cancellation retention failure")
	}
	if s.failReadiness.Load() && bytes.Equal(content, []byte("atg-local-development-artifact-readiness-v1\n")) {
		return artifact.Ref{}, errors.New("injected readiness storage failure")
	}
	var manifest serviceResultManifest
	_ = json.Unmarshal(content, &manifest)
	isManifest := manifest.SchemaVersion == "local-execution-result/v1"
	if isManifest && s.failManifest.Load() {
		return artifact.Ref{}, errors.New("injected result manifest retention failure")
	}
	if isManifest && manifest.CleanupConfirmed && s.cancelFinalPut.CompareAndSwap(true, false) {
		s.cancelFinal()
		return artifact.Ref{}, context.Canceled
	}
	ref, err := s.Store.Put(ctx, content)
	if err == nil && isManifest {
		s.mu.Lock()
		s.retained = append(s.retained, retainedServiceManifest{manifest, ref.Digest})
		s.mu.Unlock()
	}
	return ref, err
}

func (s *faultServiceArtifacts) Get(ctx context.Context, digest string, limit int64) ([]byte, error) {
	data, err := s.Store.Get(ctx, digest, limit)
	if err == nil && bytes.Contains(data, []byte(`"schema_version":"fake-cancellation/v1"`)) && s.failCancelGet.CompareAndSwap(true, false) {
		return nil, errors.New("injected transient cancellation readback failure")
	}
	return data, err
}

func (s *faultServiceArtifacts) manifests() []retainedServiceManifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]retainedServiceManifest{}, s.retained...)
}

type faultServiceRepository struct {
	Repository
	loseSourceAcknowledgement atomic.Bool
	failPing                  atomic.Bool
}

func (r *faultServiceRepository) Ping(ctx context.Context) error {
	if r.failPing.Load() {
		return errors.New("injected database readiness failure")
	}
	return r.Repository.Ping(ctx)
}

func (r *faultServiceRepository) FinishTestGenerationOperation(ctx context.Context, claim testgenflow.FlowClaim, key, state string, result json.RawMessage, diagnostic string) error {
	if err := r.Repository.FinishTestGenerationOperation(ctx, claim, key, state, result, diagnostic); err != nil {
		return err
	}
	if strings.Contains(key, ":source:") && r.loseSourceAcknowledgement.CompareAndSwap(true, false) {
		return errors.New("injected lost acknowledgement after durable finish")
	}
	return nil
}

type fakeServiceCall struct {
	kind string
	work testgenflow.WorkRequest
}

// This worker deliberately performs no OCI, subprocess, network, or model work.
// Its named in-memory source job survives Service reconstruction; all reported
// artifact and evidence references point to bytes retained in the real FileStore.
type fakeServiceWorker struct {
	store                            artifact.Store
	artifactDigest, patchDigest      string
	sourceEvidence                   string
	mu                               sync.Mutex
	jobs                             map[string]bool
	calls                            []fakeServiceCall
	launches, cleanups, cancels      int
	blockSource, pendingSource       atomic.Bool
	missingJob, failReady            atomic.Bool
	evidenceUnavailableAfterCleanup  atomic.Bool
	sourceStarted, sourceInterrupted chan struct{}
	failedPhase                      testgenflow.MavenPhase
	cleanupError                     error
	onCleanup                        func(context.Context) error
}

func (w *fakeServiceWorker) Ready(context.Context) error {
	if w.failReady.Load() {
		return errors.New("injected worker readiness failure")
	}
	return nil
}

func (w *fakeServiceWorker) ConfirmOperations(ctx context.Context, _ string, keys []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, key := range keys {
		if !w.jobs[key] || w.missingJob.Load() {
			return &testgenflow.WorkerFailure{Diagnostic: "prepared worker is missing", OutcomeUnknown: true}
		}
	}
	return nil
}

func (w *fakeServiceWorker) RunEvidence(ctx context.Context, _ string) ([]string, error) {
	w.mu.Lock()
	cleanups := w.cleanups
	w.mu.Unlock()
	if w.evidenceUnavailableAfterCleanup.Load() && cleanups > 0 {
		return nil, errors.New("worker state was already cleaned up")
	}
	if _, err := w.store.Get(ctx, w.sourceEvidence, 1<<20); err != nil {
		return nil, err
	}
	return []string{w.sourceEvidence}, nil
}

func (w *fakeServiceWorker) record(kind string, work testgenflow.WorkRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	work.ChangedTestPaths = append([]string(nil), work.ChangedTestPaths...)
	w.calls = append(w.calls, fakeServiceCall{kind, work})
}

func (w *fakeServiceWorker) ResolveSource(ctx context.Context, work testgenflow.WorkRequest) (string, error) {
	w.record("source", work)
	w.mu.Lock()
	exists := w.jobs[work.IdempotencyKey] && !w.missingJob.Load()
	if work.ReconcileOnly && !exists {
		w.mu.Unlock()
		return "", &testgenflow.WorkerFailure{Diagnostic: "prepared worker is missing", OutcomeUnknown: true}
	}
	if !work.ReconcileOnly {
		if exists {
			w.mu.Unlock()
			return "", errors.New("duplicate external source dispatch")
		}
		w.jobs[work.IdempotencyKey] = true
		w.launches++
	}
	w.mu.Unlock()
	select {
	case w.sourceStarted <- struct{}{}:
	default:
	}
	if w.blockSource.Load() {
		<-ctx.Done()
		select {
		case w.sourceInterrupted <- struct{}{}:
		default:
		}
		return "", ctx.Err()
	}
	if w.pendingSource.CompareAndSwap(true, false) {
		return "", testgenflow.ErrWorkPending
	}
	return work.Request.Revision, nil
}

func (w *fakeServiceWorker) PrepareTestArtifact(ctx context.Context, work testgenflow.WorkRequest) (testgenflow.GenerationResult, error) {
	w.record("artifact_input", work)
	if _, err := w.store.Get(ctx, work.Request.InputArtifactDigest, 1<<20); err != nil {
		return testgenflow.GenerationResult{}, err
	}
	return testgenflow.GenerationResult{ChangedTestPaths: []string{serviceTestPath}, PatchDigest: w.patchDigest, TestArtifactDigest: w.artifactDigest}, nil
}

func (w *fakeServiceWorker) RunMaven(ctx context.Context, work testgenflow.MavenWorkRequest) (testgenflow.MavenResult, error) {
	w.record("maven:"+string(work.Phase), work.WorkRequest)
	succeeded := work.Phase != w.failedPhase
	report := `<testsuite><testcase classname="example.AddedTest" name="verifies">`
	if work.Phase == testgenflow.MavenPhasePackage {
		report = `<testsuite><testcase classname="example.BaselineTest" name="baseline">`
	}
	if !succeeded {
		report += `<failure>injected assertion failure</failure>`
	}
	report += `</testcase></testsuite>`
	reportRef, err := w.store.Put(ctx, []byte(report))
	if err != nil {
		return testgenflow.MavenResult{}, err
	}
	content, _ := json.Marshal(map[string]any{"schema_version": "fake-worker-evidence/v1", "run_id": work.RunID, "phase": work.Phase, "succeeded": succeeded, "report_digest": reportRef.Digest})
	evidenceRef, err := w.store.Put(ctx, content)
	if err != nil {
		return testgenflow.MavenResult{}, err
	}
	result := testgenflow.MavenResult{Succeeded: succeeded, EvidenceRef: evidenceRef.Digest}
	if !succeeded {
		result.Diagnostic = "fake Maven assertion failed"
	}
	return result, nil
}

func (w *fakeServiceWorker) CancelRun(ctx context.Context, runID string) (string, error) {
	w.mu.Lock()
	w.cancels++
	hasJob := len(w.jobs) > 0
	w.mu.Unlock()
	if !hasJob {
		return "", &testgenflow.WorkerFailure{Diagnostic: "cannot confirm absent worker termination", OutcomeUnknown: true}
	}
	partial, err := w.store.Put(ctx, []byte("fake worker interrupted; partial evidence retained\n"))
	if err != nil {
		return "", err
	}
	content, _ := json.Marshal(map[string]any{"schema_version": "fake-cancellation/v1", "run_id": runID, "termination_confirmed": true, "evidence_refs": []string{partial.Digest}})
	ref, err := w.store.Put(ctx, content)
	return ref.Digest, err
}

func (w *fakeServiceWorker) CleanupRun(ctx context.Context, _ string) error {
	w.mu.Lock()
	w.cleanups++
	w.mu.Unlock()
	if w.onCleanup != nil {
		if err := w.onCleanup(ctx); err != nil {
			return err
		}
	}
	return w.cleanupError
}

func (w *fakeServiceWorker) snapshot() ([]fakeServiceCall, int, int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]fakeServiceCall{}, w.calls...), w.launches, w.cleanups, w.cancels
}

func operationKinds(operations []testgenflow.Operation) []string {
	kinds := make([]string, 0, len(operations))
	for _, operation := range operations {
		kinds = append(kinds, operation.Kind)
	}
	slices.Sort(kinds)
	return kinds
}

func workerCallKinds(calls []fakeServiceCall) []string {
	kinds := make([]string, 0, len(calls))
	for _, call := range calls {
		kinds = append(kinds, call.kind)
	}
	return kinds
}

func findServiceOperation(t *testing.T, operations []testgenflow.Operation, kind string) testgenflow.Operation {
	t.Helper()
	for _, op := range operations {
		if op.Kind == kind {
			return op
		}
	}
	t.Fatalf("missing %s operation in %#v", kind, operations)
	return testgenflow.Operation{}
}

func waitServiceSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for fake worker signal")
	}
}

func waitServiceStart(t *testing.T, started <-chan struct{}, result <-chan error) {
	t.Helper()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("DriveOnce returned before worker started: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for fake worker start")
	}
}

func waitServiceResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for local driver")
		return fmt.Errorf("local driver timed out")
	}
}
