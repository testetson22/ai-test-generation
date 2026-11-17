package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
)

const (
	MaxEvidenceArtifacts  = 100
	MaxResultBytes        = 1_000_000
	MaxTotalEvidenceBytes = 100_000_000
)

type Content struct {
	Artifact execution.Artifact
	Bytes    []byte
}

// Recorder durably links the provisional result before cleanup and replaces
// that link only after cleanup has a confirmed outcome.
type Recorder interface {
	RecordProvisional(context.Context, execution.ExecutionResult, execution.Artifact) error
	RecordFinalized(context.Context, execution.ExecutionResult, execution.Artifact, execution.Artifact) error
}

type Published struct {
	Result    execution.ExecutionResult
	Reference execution.Artifact
}

type Publisher struct {
	store            artifact.Store
	recorder         Recorder
	maxArtifactBytes int64
}

func NewPublisher(store artifact.Store, recorder Recorder, maxArtifactBytes int64) (*Publisher, error) {
	if store == nil || recorder == nil || maxArtifactBytes <= 0 {
		return nil, fmt.Errorf("artifact store, result recorder, and positive artifact limit are required")
	}
	return &Publisher{store: store, recorder: recorder, maxArtifactBytes: maxArtifactBytes}, nil
}

// PublishProvisional stores and verifies every referenced evidence object, then
// the result manifest, then its durable task delivery record. Cleanup must not
// proceed unless this call succeeds.
func (p *Publisher) PublishProvisional(ctx context.Context, result execution.ExecutionResult, contents []Content) (Published, error) {
	if result.Status != execution.ResultProvisional {
		return Published{}, fmt.Errorf("initial execution result must be provisional")
	}
	if err := result.Validate(); err != nil {
		return Published{}, err
	}
	references, err := resultArtifacts(result)
	if err != nil {
		return Published{}, err
	}
	if len(references) > MaxEvidenceArtifacts || len(contents) != len(references) {
		return Published{}, fmt.Errorf("evidence content must match the bounded result artifact inventory")
	}
	byID := make(map[string]Content, len(contents))
	for _, content := range contents {
		id := content.Artifact.ArtifactID
		if _, exists := byID[id]; exists {
			return Published{}, fmt.Errorf("duplicate evidence content for artifact %q", id)
		}
		byID[id] = content
	}
	var totalBytes int64
	for id, reference := range references {
		content, exists := byID[id]
		if !exists || content.Artifact != reference {
			return Published{}, fmt.Errorf("evidence content does not match artifact reference %q", id)
		}
		if int64(len(content.Bytes)) > p.maxArtifactBytes || reference.SizeBytes != int64(len(content.Bytes)) || artifact.Digest(content.Bytes) != reference.Digest {
			return Published{}, fmt.Errorf("evidence artifact %q failed size or digest verification", id)
		}
		totalBytes += int64(len(content.Bytes))
		if totalBytes > MaxTotalEvidenceBytes {
			return Published{}, fmt.Errorf("total evidence bytes exceed size limit")
		}
	}
	for _, reference := range references {
		content := byID[reference.ArtifactID]
		if err := p.putAndVerify(ctx, reference.Digest, content.Bytes); err != nil {
			return Published{}, fmt.Errorf("persist evidence artifact %q: %w", reference.ArtifactID, err)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return Published{}, fmt.Errorf("encode provisional execution result: %w", err)
	}
	if int64(len(encoded)) > MaxResultBytes {
		return Published{}, fmt.Errorf("execution result exceeds size limit")
	}
	resultRef := execution.Artifact{
		ArtifactID: result.ResultID, MediaType: execution.ExecutionResultMediaType,
		Digest: artifact.Digest(encoded), SizeBytes: int64(len(encoded)),
	}
	if err := p.putAndVerify(ctx, resultRef.Digest, encoded); err != nil {
		return Published{}, fmt.Errorf("persist provisional result manifest: %w", err)
	}
	if err := p.recorder.RecordProvisional(ctx, result, resultRef); err != nil {
		return Published{}, fmt.Errorf("record provisional result delivery: %w", err)
	}
	return Published{Result: result, Reference: resultRef}, nil
}

func (p *Publisher) Finalize(ctx context.Context, published Published, cleanupStatus string, terminationConfirmed bool, finishedAt time.Time) (execution.Artifact, error) {
	if published.Result.Status != execution.ResultProvisional || published.Reference.ArtifactID != published.Result.ResultID || published.Reference.MediaType != execution.ExecutionResultMediaType || finishedAt.IsZero() {
		return execution.Artifact{}, fmt.Errorf("valid provisional result reference and cleanup time are required")
	}
	encoded, err := p.store.Get(ctx, published.Reference.Digest, MaxResultBytes)
	if err != nil {
		return execution.Artifact{}, fmt.Errorf("load provisional result manifest: %w", err)
	}
	if int64(len(encoded)) != published.Reference.SizeBytes || artifact.Digest(encoded) != published.Reference.Digest {
		return execution.Artifact{}, fmt.Errorf("provisional result manifest failed integrity verification")
	}
	var result execution.ExecutionResult
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return execution.Artifact{}, fmt.Errorf("decode provisional result manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return execution.Artifact{}, fmt.Errorf("provisional result manifest contains trailing data")
	}
	if result.Status != execution.ResultProvisional || result.ResultID != published.Result.ResultID || result.TaskID != published.Result.TaskID || result.AttemptID != published.Result.AttemptID {
		return execution.Artifact{}, fmt.Errorf("stored provisional result identity differs from delivery")
	}
	result.Cleanup.Status = cleanupStatus
	result.Cleanup.TerminationConfirmed = terminationConfirmed
	result.Cleanup.FinishedAt = finishedAt.UTC().Format(time.RFC3339Nano)
	result.UpdatedAt = result.Cleanup.FinishedAt
	result.FinalizedAt = result.Cleanup.FinishedAt
	switch cleanupStatus {
	case "succeeded":
		if !terminationConfirmed {
			return execution.Artifact{}, fmt.Errorf("successful cleanup requires confirmed termination")
		}
		result.Status = result.ObservedOutcome
	case "not_required":
		if !terminationConfirmed || result.ObservedOutcome != execution.ResultNotReached {
			return execution.Artifact{}, fmt.Errorf("cleanup is unnecessary only when execution was not reached")
		}
		result.Status = execution.ResultNotReached
	case "failed":
		result.Status = execution.ResultFailed
		result.FailureCategory = "cleanup_failed"
		result.Cleanup.FailureCategory = "cleanup_failed"
	case "unknown":
		result.Status = execution.ResultOutcomeUnknown
	default:
		return execution.Artifact{}, fmt.Errorf("unsupported cleanup status %q", cleanupStatus)
	}
	if err := result.Validate(); err != nil {
		return execution.Artifact{}, fmt.Errorf("final execution result is invalid: %w", err)
	}
	finalBytes, err := json.Marshal(result)
	if err != nil {
		return execution.Artifact{}, fmt.Errorf("encode final execution result: %w", err)
	}
	if int64(len(finalBytes)) > MaxResultBytes {
		return execution.Artifact{}, fmt.Errorf("final execution result exceeds size limit")
	}
	finalRef := execution.Artifact{
		ArtifactID: result.ResultID, MediaType: execution.ExecutionResultMediaType,
		Digest: artifact.Digest(finalBytes), SizeBytes: int64(len(finalBytes)),
	}
	if err := p.putAndVerify(ctx, finalRef.Digest, finalBytes); err != nil {
		return execution.Artifact{}, fmt.Errorf("persist finalized result manifest: %w", err)
	}
	if err := p.recorder.RecordFinalized(ctx, result, published.Reference, finalRef); err != nil {
		return execution.Artifact{}, fmt.Errorf("record finalized result delivery: %w", err)
	}
	return finalRef, nil
}

func (p *Publisher) putAndVerify(ctx context.Context, digest string, content []byte) error {
	reference, err := p.store.Put(ctx, content)
	if err != nil {
		return err
	}
	if reference.Digest != digest || reference.SizeBytes != int64(len(content)) {
		return fmt.Errorf("artifact store returned mismatched identity")
	}
	stored, err := p.store.Get(ctx, digest, int64(len(content)))
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, content) {
		return artifact.ErrCorrupt
	}
	return nil
}

func resultArtifacts(result execution.ExecutionResult) (map[string]execution.Artifact, error) {
	artifacts := make(map[string]execution.Artifact)
	for _, reference := range result.Artifacts {
		if err := addArtifact(artifacts, reference); err != nil {
			return nil, err
		}
	}
	for _, reference := range result.Tests.ReportArtifacts {
		if err := addArtifact(artifacts, reference); err != nil {
			return nil, err
		}
	}
	for _, phase := range result.Phases {
		for _, reference := range phase.Artifacts {
			if err := addArtifact(artifacts, reference); err != nil {
				return nil, err
			}
		}
	}
	return artifacts, nil
}

func addArtifact(artifacts map[string]execution.Artifact, reference execution.Artifact) error {
	if previous, exists := artifacts[reference.ArtifactID]; exists {
		if previous != reference {
			return fmt.Errorf("conflicting metadata for evidence artifact %q", reference.ArtifactID)
		}
		return nil
	}
	artifacts[reference.ArtifactID] = reference
	return nil
}
