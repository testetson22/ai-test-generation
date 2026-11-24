package localworker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

type Config struct {
	Engine    string
	Image     string
	Profile   Profile
	Artifacts artifact.Store
}
type Worker struct {
	mu             sync.Mutex
	exec           runner
	image, imageID string
	profile        Profile
	store          artifact.Store
	boundDigest    atomic.Value
	operationGate  chan struct{}
}

// ProfileDigest identifies the retained profile AND immutable local image ID.
// It is empty until Ready has validated and durably retained that binding.
func (w *Worker) ProfileDigest() string {
	value := w.boundDigest.Load()
	if value == nil {
		return ""
	}
	return value.(string)
}

func boundProfile(p Profile, imageID string) []byte {
	raw, _ := json.Marshal(struct {
		SchemaVersion string          `json:"schema_version"`
		ProfileDigest string          `json:"profile_digest"`
		Profile       json.RawMessage `json:"profile"`
		LocalImageID  string          `json:"local_image_id"`
	}{"1", p.Digest, json.RawMessage(p.raw), imageID})
	return raw
}

func New(c Config) (*Worker, error) {
	if err := c.Profile.validate(); err != nil {
		return nil, err
	}
	if c.Artifacts == nil || len(c.Profile.raw) == 0 || artifact.Digest(c.Profile.raw) != c.Profile.Digest {
		return nil, fmt.Errorf("validated profile and artifact store are required")
	}
	// Keep the exact loaded profile, not mutable maps supplied after loading.
	p, err := DecodeProfile(c.Profile.raw)
	if err != nil {
		return nil, err
	}
	if c.Engine == "" {
		c.Engine = "podman"
	}
	if c.Image == "" {
		c.Image = DefaultImage
	}
	return &Worker{exec: engineRunner(c.Engine), image: c.Image, profile: p, store: c.Artifacts, operationGate: make(chan struct{}, 1)}, nil
}

func digestJSON(v any) string { raw, _ := json.Marshal(v); return artifact.Digest(raw) }
func containerName(key string) string {
	return "atg-worker-" + strings.TrimPrefix(artifact.Digest([]byte(key)), "sha256:")
}
func runLabel(run string) string { return strings.TrimPrefix(artifact.Digest([]byte(run)), "sha256:") }
func unknown(message string, err error) error {
	return &testgenflow.WorkerFailure{Diagnostic: message, OutcomeUnknown: true, Cause: err}
}

func (w *Worker) validateRequest(r testgenflow.WorkRequest) error {
	p := w.profile
	if (r.Request.InputMode != testgenflow.InputModeArtifactInput && r.Request.InputMode != testgenflow.InputModeModelGeneration) || r.Request.CompletionMode != testgenflow.CompletionModePatchOnly {
		return fmt.Errorf("local worker requires an explicit development input mode and patch-only policy")
	}
	if w.ProfileDigest() == "" || r.RunID == "" || r.IdempotencyKey == "" || len(r.RunID) > 256 || len(r.IdempotencyKey) > 1024 || r.Request.RepositoryURL != p.RepositoryURL || r.Request.Revision != p.Commit || r.Request.ProfileID != p.ID || r.Request.ProfileVersion != p.Version || r.Request.ProfileDigest != w.ProfileDigest() || r.Request.Subdirectory != p.Subdirectory || r.Request.VerifyCommit {
		return fmt.Errorf("request does not match the fixed unsigned public-source development profile")
	}
	if r.ResolvedCommit != "" && r.ResolvedCommit != p.Commit {
		return fmt.Errorf("resolved source differs from profile pin")
	}
	return nil
}

func (w *Worker) ResolveSource(ctx context.Context, r testgenflow.WorkRequest) (string, error) {
	result, err := w.operation(ctx, r, "source", TestArtifact{}, w.profile.primarySubject())
	if err != nil {
		return "", err
	}
	if !result.Succeeded {
		return "", &evidenceFailure{ref: result.EvidenceRef, failure: &testgenflow.WorkerFailure{Diagnostic: result.Diagnostic}}
	}
	return w.profile.Commit, nil
}

type evidenceFailure struct {
	ref     string
	failure *testgenflow.WorkerFailure
}

func (e *evidenceFailure) Error() string            { return e.failure.Error() }
func (e *evidenceFailure) Unwrap() error            { return e.failure }
func (e *evidenceFailure) RetainedEvidence() string { return e.ref }

func (w *Worker) RunMaven(ctx context.Context, r testgenflow.MavenWorkRequest) (testgenflow.MavenResult, error) {
	var a TestArtifact
	if !slices.Contains([]testgenflow.MavenPhase{testgenflow.MavenPhasePackage, testgenflow.MavenPhaseFocusedTests, testgenflow.MavenPhaseFinalVerify}, r.Phase) {
		return testgenflow.MavenResult{}, fmt.Errorf("unsupported Maven phase")
	}
	if r.Phase != testgenflow.MavenPhasePackage {
		var err error
		a, err = w.admitted(ctx, r.WorkRequest)
		if err != nil {
			return testgenflow.MavenResult{}, err
		}
	}
	if w.profile.ExecutionPair != nil && (r.Phase == testgenflow.MavenPhaseFocusedTests || r.Phase == testgenflow.MavenPhaseFinalVerify) {
		return w.operationPair(ctx, r, string(r.Phase), a)
	}
	return w.operation(ctx, r.WorkRequest, string(r.Phase), a, w.profile.primarySubject())
}

// SubjectOutcome records one immutable subject's execution outcome. The
// provider-neutral contract type lives in testgenflow.
type SubjectOutcome = testgenflow.SubjectOutcome

// PairEvidence is the differential phase document: the same admitted test
// artifact executed against both pinned subjects, with per-subject outcomes.
type PairEvidence = testgenflow.PairEvidence

// operationPair executes the same admitted test artifact against both pinned
// subjects. The phase succeeds when the candidate subject succeeds; the base
// outcome is recorded either way so a base-failure/candidate-success pair is a
// successful differential observation rather than a repair trigger.
func (w *Worker) operationPair(ctx context.Context, r testgenflow.MavenWorkRequest, phase string, a TestArtifact) (testgenflow.MavenResult, error) {
	subjects := w.profile.executionSubjects()
	outcomes := make([]SubjectOutcome, 0, len(subjects))
	for _, subject := range subjects {
		result, err := w.operation(ctx, r.WorkRequest, phase, a, subject)
		if err != nil {
			return testgenflow.MavenResult{}, err
		}
		outcomes = append(outcomes, SubjectOutcome{Subject: subject.name, Commit: subject.commit, Succeeded: result.Succeeded, EvidenceRef: result.EvidenceRef, Diagnostic: result.Diagnostic})
	}
	base, candidate := outcomes[0], outcomes[1]
	document := PairEvidence{SchemaVersion: "1", Phase: phase, RunID: r.RunID, ProfileDigest: w.ProfileDigest(), TestArtifactDigest: r.TestArtifactDigest, Subjects: outcomes, Succeeded: candidate.Succeeded, DifferentialObserved: !base.Succeeded && candidate.Succeeded}
	if len(a.Files) > 0 {
		document.PatchDigest = digestJSON(a.PatchContent())
	}
	if !candidate.Succeeded {
		document.Diagnostic = "candidate subject failed; inspect per-subject evidence"
	}
	content, err := json.Marshal(document)
	if err != nil {
		return testgenflow.MavenResult{}, err
	}
	ref, err := w.put(ctx, content)
	if err != nil {
		return testgenflow.MavenResult{}, err
	}
	return testgenflow.MavenResult{Succeeded: document.Succeeded, EvidenceRef: ref, Diagnostic: document.Diagnostic}, nil
}

func (w *Worker) operation(ctx context.Context, r testgenflow.WorkRequest, phase string, a TestArtifact, subject executionSubject) (testgenflow.MavenResult, error) {
	// Serialize workload memory, not health checks or cancellation. Acquisition
	// observes cancellation instead of waiting indefinitely on a mutex.
	select {
	case w.operationGate <- struct{}{}:
		defer func() { <-w.operationGate }()
	case <-ctx.Done():
		return testgenflow.MavenResult{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return testgenflow.MavenResult{}, err
	}
	if err := w.validateRequest(r); err != nil {
		return testgenflow.MavenResult{}, err
	}
	w.mu.Lock()
	imageID := w.imageID
	w.mu.Unlock()
	if imageID == "" {
		return testgenflow.MavenResult{}, fmt.Errorf("worker Ready must succeed before operations")
	}
	input := digestJSON(struct {
		Request                                testgenflow.WorkRequest
		Phase, Profile, Image, Subject, Commit string
	}{r, phase, w.ProfileDigest(), imageID, subject.name, subject.commit})
	name := containerName(r.IdempotencyKey + subjectContainerSuffix(subject.name))
	state, exists, err := w.inspect(ctx, name)
	if err != nil {
		return testgenflow.MavenResult{}, unknown("worker inspection failed", err)
	}
	if !exists {
		if r.ReconcileOnly {
			return testgenflow.MavenResult{}, unknown("journaled worker is missing; refusing to recreate", nil)
		}
		selector := w.profile.testSelector(phase, a)
		args := append([]string{"create"}, w.sandboxArgs(name, runLabel(r.RunID), input, "bridge", w.profile.DeadlineSeconds+w.profile.RetentionSeconds)...)
		if subject.name != "" {
			args = append(args, "--label=io.lightpath.atg.subject="+subject.name)
		}
		args = append(args, imageID, "run", phase, selector, w.profile.RepositoryURL, subject.commit)
		_, createErr := w.exec(ctx, nil, 4096, args...)
		state, exists, err = w.inspect(ctx, name)
		if err != nil || !exists {
			return testgenflow.MavenResult{}, unknown("worker creation acknowledgement is unknown", errorsJoin(createErr, err))
		}
	}
	if state.Config.Labels["io.lightpath.atg.worker"] != w.profile.WorkerRecipe || state.Config.Labels["io.lightpath.atg.run"] != runLabel(r.RunID) || state.Config.Labels["io.lightpath.atg.input"] != input || state.Config.Labels["io.lightpath.atg.subject"] != subject.name || state.Image != imageID {
		return testgenflow.MavenResult{}, unknown("named worker input or image identity conflict", nil)
	}
	if state.State.Status == "created" || state.State.Status == "configured" {
		_, startErr := w.exec(ctx, nil, 4096, "start", state.ID)
		state, exists, err = w.inspect(ctx, state.ID)
		if err != nil || !exists || (!state.State.Running && state.State.Status != "exited") {
			return testgenflow.MavenResult{}, unknown("worker start acknowledgement is unknown", errorsJoin(startErr, err))
		}
	}
	deadline := state.State.StartedAt.Add(time.Duration(w.profile.DeadlineSeconds) * time.Second)
	if state.State.StartedAt.IsZero() {
		return testgenflow.MavenResult{}, unknown("worker start time missing", nil)
	}
	timer := time.NewTicker(250 * time.Millisecond)
	defer timer.Stop()
	for {
		if !state.State.Running {
			return w.receiptResult(ctx, state, input)
		}
		status, statusErr := w.exec(ctx, nil, 128, "exec", state.ID, "/bin/bash", "/opt/atg/worker.sh", "status")
		if statusErr != nil {
			return testgenflow.MavenResult{}, unknown("worker observation failed", statusErr)
		}
		value := strings.TrimSpace(string(status))
		if code, parseErr := strconv.Atoi(value); parseErr == nil {
			result, evidenceErr := w.collect(ctx, state, input, phase, code, a, r.TestArtifactDigest, false, subject)
			if evidenceErr != nil {
				return testgenflow.MavenResult{}, unknown("completed worker evidence retention failed", evidenceErr)
			}
			if err := w.seal(ctx, state.ID, result.EvidenceRef, false); err != nil {
				return testgenflow.MavenResult{}, unknown("evidence retained but worker completion acknowledgement lost", err)
			}
			return result, nil
		}
		if value != "pending" && value != "input" {
			return testgenflow.MavenResult{}, unknown("invalid worker protocol status", nil)
		}
		if value == "input" && len(a.Files) > 0 {
			if _, err = w.exec(ctx, a.archive(), MaxTestArtifactBytes, "exec", "-i", state.ID, "/bin/bash", "/opt/atg/worker.sh", "admit", r.TestArtifactDigest); err != nil {
				return testgenflow.MavenResult{}, unknown("artifact admission failed or acknowledgement lost", err)
			}
		}
		if time.Now().After(deadline) {
			return testgenflow.MavenResult{}, unknown("worker operation deadline exceeded", w.cancelContainer(ctx, state, input))
		}
		select {
		case <-ctx.Done():
			return testgenflow.MavenResult{}, ctx.Err()
		case <-timer.C:
		}
		state, exists, err = w.inspect(ctx, state.ID)
		if err != nil || !exists {
			return testgenflow.MavenResult{}, unknown("worker disappeared while observing", err)
		}
	}
}

func subjectContainerSuffix(subject string) string {
	if subject == "" {
		return ""
	}
	return ":" + subject
}

func errorsJoin(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
