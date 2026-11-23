package localservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/httpapi"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

type operations struct {
	service *Service
	claim   testgenflow.FlowClaim
}

func (o *operations) execute(ctx context.Context, key, kind string, input any, call func(bool) (any, error), output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	digest := artifact.Digest(body)
	op, created, err := o.service.config.Repository.BeginTestGenerationOperation(ctx, o.claim, key, kind, digest)
	if err != nil {
		if errors.Is(err, testgenflow.ErrOperationConflict) {
			return &testgenflow.WorkerFailure{Diagnostic: "operation identity conflicts with recorded inputs", OutcomeUnknown: true, Cause: err}
		}
		return fmt.Errorf("%w: operation intent unavailable", testgenflow.ErrWorkPending)
	}
	if op.State == "completed" {
		return json.Unmarshal(op.Result, output)
	}
	if op.State == "unknown" || op.State == "failed" {
		return o.recordedFailure(ctx, kind, op.State, op.Diagnostic, op.Result, nil)
	}
	result, err := call(!created)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, testgenflow.ErrWorkPending) {
			return err
		}
		state := "failed"
		var failure *testgenflow.WorkerFailure
		if errors.As(err, &failure) && failure.OutcomeUnknown {
			state = "unknown"
		}
		diagnostic := "local worker operation failed; inspect retained worker evidence"
		if errors.As(err, &failure) {
			diagnostic = singleLine(failure.Diagnostic)
		}
		var failureResult json.RawMessage
		var retained interface{ RetainedEvidence() string }
		if errors.As(err, &retained) {
			failureResult, _ = json.Marshal(map[string]string{"evidence_ref": retained.RetainedEvidence()})
		}
		if saveErr := o.service.config.Repository.FinishTestGenerationOperation(ctx, o.claim, key, state, failureResult, diagnostic); saveErr != nil {
			return fmt.Errorf("%w: operation outcome could not be acknowledged", testgenflow.ErrWorkPending)
		}
		return o.recordedFailure(ctx, kind, state, diagnostic, failureResult, err)
	}
	body, err = json.Marshal(result)
	if err != nil {
		return err
	}
	if err := o.service.config.Repository.FinishTestGenerationOperation(ctx, o.claim, key, "completed", body, ""); err != nil {
		return fmt.Errorf("%w: operation result could not be acknowledged", testgenflow.ErrWorkPending)
	}
	return json.Unmarshal(body, output)
}

func (o *operations) recordedFailure(ctx context.Context, kind, state, diagnostic string, result json.RawMessage, cause error) error {
	failure := &testgenflow.WorkerFailure{Diagnostic: diagnostic, OutcomeUnknown: state == "unknown", Cause: cause}
	var retained struct {
		EvidenceRef string `json:"evidence_ref"`
	}
	if json.Unmarshal(result, &retained) != nil || retained.EvidenceRef == "" {
		return failure
	}
	if _, err := o.service.config.Artifacts.Get(ctx, retained.EvidenceRef, httpapi.MaxDevelopmentArtifactBytes); err != nil {
		return &testgenflow.WorkerFailure{Diagnostic: "recorded worker failure evidence is unavailable", OutcomeUnknown: true, Cause: err}
	}
	if kind == "source" && state == "failed" {
		if err := o.service.config.Worker.CleanupRun(ctx, o.claim.RunID); err != nil {
			failure = &testgenflow.WorkerFailure{Diagnostic: "source failure retained but cleanup is unconfirmed", OutcomeUnknown: true, Cause: err}
		}
	}
	return &retainedFailure{ref: retained.EvidenceRef, failure: failure}
}

func singleLine(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 512 {
		value = value[:512]
	}
	if value == "" {
		return "local worker operation failed"
	}
	return value
}

func (o *operations) ResolveSource(ctx context.Context, work testgenflow.WorkRequest) (string, error) {
	var result string
	err := o.execute(ctx, work.IdempotencyKey, "source", work, func(reconcile bool) (any, error) {
		work.ReconcileOnly = work.ReconcileOnly || reconcile
		return o.service.config.Worker.ResolveSource(ctx, work)
	}, &result)
	return result, err
}

func (o *operations) PrepareTestArtifact(ctx context.Context, work testgenflow.WorkRequest) (testgenflow.GenerationResult, error) {
	var result testgenflow.GenerationResult
	err := o.execute(ctx, work.IdempotencyKey, "artifact_input", work, func(reconcile bool) (any, error) {
		work.ReconcileOnly = work.ReconcileOnly || reconcile
		return o.service.config.Worker.PrepareTestArtifact(ctx, work)
	}, &result)
	return result, err
}

func (o *operations) RunMaven(ctx context.Context, work testgenflow.MavenWorkRequest) (testgenflow.MavenResult, error) {
	var result testgenflow.MavenResult
	err := o.execute(ctx, work.IdempotencyKey, "maven:"+string(work.Phase), work, func(reconcile bool) (any, error) {
		work.ReconcileOnly = work.ReconcileOnly || reconcile
		return o.service.config.Worker.RunMaven(ctx, work)
	}, &result)
	if err == nil && !result.Succeeded {
		if cleanupErr := o.service.config.Worker.CleanupRun(ctx, work.RunID); cleanupErr != nil {
			return result, &testgenflow.WorkerFailure{Diagnostic: "failed test evidence retained but cleanup is unconfirmed", OutcomeUnknown: true, Cause: cleanupErr}
		}
	}
	return result, err
}

func (o *operations) PublishPatch(ctx context.Context, work testgenflow.WorkRequest) (string, error) {
	var result string
	err := o.execute(ctx, work.IdempotencyKey, "publish_patch", work, func(bool) (any, error) { return o.publish(ctx, work) }, &result)
	return result, err
}

func (o *operations) cancel(ctx context.Context, run testgenflow.Run) error {
	key := run.RunID + "/cancel"
	var evidence string
	err := o.execute(ctx, key, "cancel", struct {
		RunID         string
		RequestDigest string
	}{run.RunID, run.RequestDigest}, func(bool) (any, error) {
		ops, err := o.service.config.Repository.ListTestGenerationOperations(ctx, run.RunID)
		if err != nil {
			return nil, fmt.Errorf("%w: cancellation inventory unavailable", testgenflow.ErrWorkPending)
		}
		var prepared []string
		external := 0
		for _, op := range ops {
			if op.Kind == "source" || strings.HasPrefix(op.Kind, "maven:") {
				external++
				if op.State == "prepared" || op.State == "unknown" {
					prepared = append(prepared, op.Key)
				}
			}
		}
		if external == 0 {
			data, _ := json.Marshal(map[string]any{"schema_version": "local-cancellation/v1", "run_id": run.RunID, "termination_confirmed": true, "external_operations_dispatched": false})
			ref, err := o.service.config.Artifacts.Put(ctx, data)
			if err == nil {
				_, err = o.service.config.Artifacts.Get(ctx, ref.Digest, 4096)
			}
			if err != nil {
				return nil, fmt.Errorf("%w: cancellation manifest retention is unavailable", testgenflow.ErrWorkPending)
			}
			return ref.Digest, err
		}
		if err := o.service.config.Worker.ConfirmOperations(ctx, run.RunID, prepared); err != nil {
			return nil, err
		}
		ref, err := o.service.config.Worker.CancelRun(ctx, run.RunID)
		if err == nil {
			if _, err = o.service.config.Artifacts.Get(ctx, ref, httpapi.MaxDevelopmentArtifactBytes); err != nil {
				return nil, fmt.Errorf("%w: cancellation manifest readback is unavailable", testgenflow.ErrWorkPending)
			}
		}
		return ref, err
	}, &evidence)
	if err != nil {
		var failure *testgenflow.WorkerFailure
		if errors.As(err, &failure) {
			hash := sha256.Sum256([]byte(key + "/unknown"))
			_, saveErr := o.service.config.Repository.ApplyClaimedTestGenerationEvent(ctx, o.claim, testgenflow.Event{
				ID: "cancel-unknown:" + hex.EncodeToString(hash[:]), ExpectedRevision: run.Revision,
				Type: testgenflow.EventOutcomeUnknown, OccurredAt: time.Now().UTC(), Diagnostic: singleLine(failure.Diagnostic),
			})
			return saveErr
		}
		return err
	}
	if err := o.service.config.Worker.CleanupRun(ctx, run.RunID); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(key))
	_, err = o.service.config.Repository.ApplyClaimedTestGenerationEvent(ctx, o.claim, testgenflow.Event{
		ID: "cancelled:" + hex.EncodeToString(hash[:]), ExpectedRevision: run.Revision,
		Type: testgenflow.EventCancelled, OccurredAt: time.Now().UTC(), TerminationConfirmed: true, EvidenceRef: evidence,
	})
	return err
}

func (o *operations) publish(ctx context.Context, work testgenflow.WorkRequest) (string, error) {
	var provisional string
	err := o.execute(ctx, work.IdempotencyKey+"/evidence", "publication_evidence", work, func(bool) (any, error) {
		return o.retainResult(ctx, work)
	}, &provisional)
	if err != nil {
		return "", err
	}
	data, err := o.service.config.Artifacts.Get(ctx, provisional, 1<<20)
	if err != nil {
		return "", err
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	if err := o.service.config.Worker.CleanupRun(ctx, work.RunID); err != nil {
		return "", &retainedFailure{ref: provisional, failure: &testgenflow.WorkerFailure{Diagnostic: "worker cleanup is unconfirmed; provisional evidence " + provisional, OutcomeUnknown: true, Cause: err}}
	}
	manifest["cleanup_confirmed"] = true
	manifest["provisional_ref"] = provisional
	data, err = json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	ref, err := o.service.config.Artifacts.Put(ctx, data)
	if err == nil {
		_, err = o.service.config.Artifacts.Get(ctx, ref.Digest, 1<<20)
	}
	return ref.Digest, err
}

func (o *operations) retainResult(ctx context.Context, work testgenflow.WorkRequest) (string, error) {
	if work.TestArtifactDigest == "" || work.PatchDigest == "" || len(work.ChangedTestPaths) == 0 {
		return "", errors.New("patch publication requires an accepted candidate")
	}
	for _, ref := range []string{work.TestArtifactDigest, work.PatchDigest} {
		if _, err := o.service.config.Artifacts.Get(ctx, ref, 32<<20); err != nil {
			return "", err
		}
	}
	ops, err := o.service.config.Repository.ListTestGenerationOperations(ctx, work.RunID)
	if err != nil {
		return "", err
	}
	retained := []testgenflow.Operation{}
	phases := map[string]bool{}
	for _, op := range ops {
		if op.Kind == "publish_patch" || op.Kind == "publication_evidence" {
			continue
		}
		if op.State != "completed" {
			return "", errors.New("cannot finalize a run with unresolved operations")
		}
		if strings.HasPrefix(op.Kind, "maven:") {
			var result testgenflow.MavenResult
			if json.Unmarshal(op.Result, &result) != nil || !result.Succeeded || result.EvidenceRef == "" {
				return "", errors.New("Maven phase lacks successful retained evidence")
			}
			if _, err := o.service.config.Artifacts.Get(ctx, result.EvidenceRef, 32<<20); err != nil {
				return "", err
			}
		}
		phases[op.Kind] = true
		retained = append(retained, op)
	}
	for _, kind := range []string{"source", "artifact_input", "maven:package", "maven:focused_tests", "maven:verify"} {
		if !phases[kind] {
			return "", fmt.Errorf("missing completed %s operation", kind)
		}
	}
	evidence, err := o.service.config.Worker.RunEvidence(ctx, work.RunID)
	if err != nil {
		return "", err
	}
	for _, ref := range evidence {
		if _, err := o.service.config.Artifacts.Get(ctx, ref, httpapi.MaxDevelopmentArtifactBytes); err != nil {
			return "", err
		}
	}
	manifest := struct {
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
	}{"local-execution-result/v1", "artifact_input_patch_only", work.RunID, work.Request, work.ResolvedCommit, work.TestArtifactDigest, work.PatchDigest, work.ChangedTestPaths, retained, evidence, false}
	put := func() (string, error) {
		data, err := json.Marshal(manifest)
		if err != nil {
			return "", err
		}
		if len(data) > 1<<20 {
			return "", artifact.ErrTooLarge
		}
		ref, err := o.service.config.Artifacts.Put(ctx, data)
		if err == nil {
			_, err = o.service.config.Artifacts.Get(ctx, ref.Digest, 1<<20)
		}
		return ref.Digest, err
	}
	return put()
}

type retainedFailure struct {
	ref     string
	failure *testgenflow.WorkerFailure
}

func (e *retainedFailure) Error() string            { return e.failure.Error() }
func (e *retainedFailure) Unwrap() error            { return e.failure }
func (e *retainedFailure) RetainedEvidence() string { return e.ref }
