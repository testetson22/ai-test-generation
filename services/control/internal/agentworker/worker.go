package agentworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

var ErrExternalOperationsDisabled = errors.New("generation external operations are disabled")

type Config struct {
	Engine          string
	Image           string
	Policy          Policy
	Artifacts       artifact.Store
	SourceEvidence  func(context.Context, string) ([]string, error)
	CredentialFile  string
	AllowModelCalls bool
}

type Worker struct {
	mu                           sync.Mutex
	exec                         commandRunner
	image, imageID, policyDigest string
	policy                       Policy
	store                        artifact.Store
	sourceEvidence               func(context.Context, string) ([]string, error)
	credentialFile               string
	allowModelCalls              bool
	secrets                      []string
	operationGate                chan struct{}
}

type StageRequest struct {
	Work                 testgenflow.WorkRequest
	Phase                string
	AnalysisRef          string
	PlanRef              string
	PreviousCandidateRef string
}
type StageResult struct {
	ArtifactRef string `json:"artifact_ref"`
	Status      string `json:"status"`
	Diagnostic  string `json:"diagnostic,omitempty"`
}

func New(c Config) (*Worker, error) {
	if c.Artifacts == nil || c.SourceEvidence == nil || len(c.Policy.raw) == 0 || artifact.Digest(c.Policy.raw) != c.Policy.Digest || strings.TrimSpace(c.Image) == "" || strings.HasPrefix(c.Image, "-") {
		return nil, fmt.Errorf("generation requires validated policy, explicit local image, artifact store and source evidence")
	}
	p, err := DecodePolicy(c.Policy.raw)
	if err != nil {
		return nil, err
	}
	if c.Engine == "" {
		c.Engine = "podman"
	}
	return &Worker{exec: (OCIBackend{Engine: c.Engine}).command, image: c.Image, policy: p, store: c.Artifacts, sourceEvidence: c.SourceEvidence, credentialFile: c.CredentialFile, allowModelCalls: c.AllowModelCalls, operationGate: make(chan struct{}, 1)}, nil
}

func (w *Worker) allowed() error {
	if !w.allowModelCalls {
		return ErrExternalOperationsDisabled
	}
	return nil
}
func (w *Worker) PolicyDigest() string  { w.mu.Lock(); defer w.mu.Unlock(); return w.policyDigest }
func (w *Worker) ProfileDigest() string { return w.PolicyDigest() }
func (w *Worker) MetadataRef() string   { return w.PolicyDigest() }
func containerName(key string) string {
	return "atg-generation-" + strings.TrimPrefix(artifact.Digest([]byte(key)), "sha256:")
}
func runLabel(id string) string     { return strings.TrimPrefix(artifact.Digest([]byte(id)), "sha256:") }
func digestBytes(raw []byte) string { return artifact.Digest(raw) }
func unknown(message string) error {
	return &testgenflow.WorkerFailure{Diagnostic: message, OutcomeUnknown: true}
}

func (w *Worker) put(ctx context.Context, raw []byte) (string, error) {
	ref, err := w.store.Put(ctx, raw)
	if err != nil {
		return "", fmt.Errorf("generation artifact retention failed")
	}
	if ref.Digest != artifact.Digest(raw) || ref.SizeBytes != int64(len(raw)) {
		return "", artifact.ErrCorrupt
	}
	got, err := w.get(ctx, ref.Digest, int64(len(raw))+1)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(got, raw) {
		return "", artifact.ErrCorrupt
	}
	return ref.Digest, nil
}
func (w *Worker) get(ctx context.Context, ref string, limit int64) ([]byte, error) {
	if !digestPattern.MatchString(ref) {
		return nil, artifact.ErrInvalidDigest
	}
	raw, err := w.store.Get(ctx, ref, limit)
	if err != nil {
		return nil, fmt.Errorf("generation artifact unavailable")
	}
	if int64(len(raw)) > limit {
		return nil, artifact.ErrTooLarge
	}
	if artifact.Digest(raw) != ref {
		return nil, artifact.ErrCorrupt
	}
	return raw, nil
}

func (w *Worker) task(ctx context.Context, r StageRequest) (Task, []byte, error) {
	x := r.Work
	if w.PolicyDigest() == "" || x.RunID == "" || len(x.RunID) > 256 || x.IdempotencyKey == "" || len(x.IdempotencyKey) > 1024 || x.Attempt < 1 || x.Attempt > 100 || !commitPattern.MatchString(x.ResolvedCommit) || !slices.Contains([]string{"analyze", "plan", "code"}, r.Phase) || x.Request.InputMode != testgenflow.InputModeModelGeneration || x.Request.CompletionMode != testgenflow.CompletionModePatchOnly || x.Request.Generation == nil || x.Request.Generation.PolicyDigest != w.PolicyDigest() || !slices.Equal(x.Request.Generation.ContextPaths, w.policy.ContextPaths) || !text(x.Request.Generation.Objective) || len(x.RepairFeedback) > 16384 {
		return Task{}, nil, fmt.Errorf("generation request differs from ready policy or pinned source")
	}
	src, err := w.source(ctx, x)
	if err != nil {
		return Task{}, nil, err
	}
	t := Task{SchemaVersion: "1", JobID: containerName(x.IdempotencyKey), RunID: x.RunID, Attempt: x.Attempt, Phase: r.Phase, Objective: x.Request.Generation.Objective, ProfileDigest: x.Request.ProfileDigest, PolicyDigest: w.PolicyDigest(), Harness: w.policy.Harness, Model: w.policy.Model, Source: src, Limits: w.policy.Limits, Prior: Prior{RepairFeedback: x.RepairFeedback, PreviousCandidateRef: r.PreviousCandidateRef}}
	for _, p := range []struct{ ref, phase string }{{r.AnalysisRef, "analyze"}, {r.PlanRef, "plan"}, {r.PreviousCandidateRef, "code"}} {
		if p.ref == "" {
			continue
		}
		e, prior, output, err := w.loadEvidence(ctx, p.ref)
		if err != nil || e.Partial || output.Status != "completed" || prior.RunID != t.RunID || prior.Phase != p.phase || prior.ProfileDigest != t.ProfileDigest || prior.PolicyDigest != t.PolicyDigest || prior.Source.Commit != t.Source.Commit || prior.Source.TreeID != t.Source.TreeID || prior.Source.ArtifactRef != t.Source.ArtifactRef || prior.Objective != t.Objective || (p.phase != "code" && prior.Attempt != t.Attempt) || (p.phase == "code" && prior.Attempt >= t.Attempt) {
			return Task{}, nil, fmt.Errorf("prior generation artifact identity mismatch")
		}
		if p.phase == "analyze" {
			t.Prior.AnalysisRef, t.Prior.Analysis = p.ref, output.Output
		}
		if p.phase == "plan" {
			if prior.Prior.AnalysisRef != r.AnalysisRef {
				return Task{}, nil, fmt.Errorf("plan does not bind selected analysis")
			}
			t.Prior.PlanRef, t.Prior.Plan = p.ref, output.Output
		}
	}
	if (r.Phase == "analyze" && (r.AnalysisRef != "" || r.PlanRef != "")) || (r.Phase == "plan" && (r.AnalysisRef == "" || r.PlanRef != "")) || (r.Phase == "code" && (r.AnalysisRef == "" || r.PlanRef == "")) {
		return Task{}, nil, fmt.Errorf("generation phase requires exact prior artifacts")
	}
	raw, err := json.Marshal(t)
	if err != nil || len(raw) > maxTaskBytes {
		return Task{}, nil, artifact.ErrTooLarge
	}
	return t, raw, nil
}

func (w *Worker) credential() ([]byte, error) {
	f, err := os.OpenFile(w.credentialFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("model credential file unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 8192 {
		return nil, fmt.Errorf("model credential requires a private bounded regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("model credential must be owned by current user")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(raw) > 8192 {
		return nil, fmt.Errorf("model credential read failed")
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
		return nil, fmt.Errorf("model credential must be a single nonempty key")
	}
	w.mu.Lock()
	if !slices.Contains(w.secrets, value) {
		w.secrets = append(w.secrets, value)
	}
	w.mu.Unlock()
	return []byte(value), nil
}

func (w *Worker) redact(raw []byte) []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.secrets {
		raw = bytes.ReplaceAll(raw, []byte(s), []byte("[REDACTED]"))
	}
	return raw
}

func (w *Worker) containsSecret(raw []byte) bool {
	if !bytes.Equal(raw, w.redact(raw)) {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var has func(any) bool
	has = func(v any) bool {
		switch item := v.(type) {
		case string:
			return !bytes.Equal([]byte(item), w.redact([]byte(item)))
		case []any:
			for _, v := range item {
				if has(v) {
					return true
				}
			}
		case map[string]any:
			for k, v := range item {
				if has(k) || has(v) {
					return true
				}
			}
		}
		return false
	}
	return has(value)
}

func (w *Worker) RunStage(ctx context.Context, r StageRequest) (StageResult, error) {
	if err := w.allowed(); err != nil {
		return StageResult{}, err
	}
	select {
	case w.operationGate <- struct{}{}:
		defer func() { <-w.operationGate }()
	case <-ctx.Done():
		return StageResult{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return StageResult{}, err
	}
	t, raw, err := w.task(ctx, r)
	if err != nil {
		return StageResult{}, err
	}
	input := artifact.Digest(raw)
	w.mu.Lock()
	frozen := w.imageID
	w.mu.Unlock()
	state, exists, err := w.inspect(ctx, t.JobID)
	if err != nil {
		return StageResult{}, unknown("generation worker inspection failed")
	}
	if !exists && r.Work.ReconcileOnly {
		return StageResult{}, unknown("journaled generation worker is missing; refusing to recreate")
	}
	if exists && !w.owns(state, t.RunID, input, t.Phase) {
		return StageResult{}, unknown("named generation worker identity conflict")
	}
	if exists && !state.State.Running && !neverStarted(state) {
		return w.receiptResult(ctx, state, input)
	}
	key, err := w.credential()
	if err != nil {
		return StageResult{}, err
	}
	defer clear(key)
	if w.containsSecret(raw) {
		return StageResult{}, fmt.Errorf("credential contamination in generation input")
	}
	if _, err = w.put(ctx, raw); err != nil {
		return StageResult{}, err
	}
	if !exists {
		args := append([]string{"create"}, w.sandbox(t.JobID, t.RunID, input, t.Phase, "bridge", t.Limits.TimeoutSeconds+120)...)
		args = append(args, frozen, "run", input, fmt.Sprint(t.Limits.TimeoutSeconds), fmt.Sprint(t.Limits.MaxOutputBytes))
		_, _ = w.exec(ctx, nil, 4096, args...)
		state, exists, err = w.inspect(ctx, t.JobID)
		if err != nil || !exists {
			return StageResult{}, unknown("generation creation acknowledgement is unknown")
		}
	}
	if !w.owns(state, t.RunID, input, t.Phase) {
		return StageResult{}, unknown("named generation worker identity conflict")
	}
	if neverStarted(state) {
		_, _ = w.exec(ctx, nil, 4096, "start", state.ID)
		state, exists, err = w.inspect(ctx, state.ID)
		if err != nil || !exists || (!state.State.Running && state.State.Status != "exited") {
			return StageResult{}, unknown("generation start acknowledgement is unknown")
		}
	}
	if state.State.StartedAt.IsZero() {
		return StageResult{}, unknown("generation worker has no start identity")
	}
	deadline := state.State.StartedAt.Add(time.Duration(t.Limits.TimeoutSeconds+30) * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if !state.State.Running {
			return w.receiptResult(ctx, state, input)
		}
		status, err := w.exec(ctx, nil, 128, "exec", state.ID, "/bin/sh", "/opt/atg/entrypoint.sh", "status")
		if err != nil {
			return StageResult{}, unknown("generation worker observation failed")
		}
		switch strings.TrimSpace(string(status)) {
		case "task":
			// Reobserve the same marker after a lost admission acknowledgement.
			_, _ = w.exec(ctx, raw, 4096, "exec", "-i", state.ID, "/bin/sh", "/opt/atg/entrypoint.sh", "admit", input)
		case "credential":
			_, _ = w.exec(ctx, key, 4096, "exec", "-i", state.ID, "/bin/sh", "/opt/atg/entrypoint.sh", "credential")
		case "pending":
		case "complete":
			result, err := w.collect(ctx, state, t, raw, false)
			if err != nil {
				return StageResult{}, unknown("completed generation evidence retention failed")
			}
			if err = w.seal(ctx, state, result.ArtifactRef, false); err != nil {
				return StageResult{}, unknown("generation evidence retained but completion acknowledgement unknown")
			}
			return result, nil
		default:
			return StageResult{}, unknown("invalid generation wrapper status")
		}
		if time.Now().After(deadline) {
			_ = w.cancelContainer(ctx, state, t.RunID)
			return StageResult{}, unknown("generation operation deadline exceeded")
		}
		select {
		case <-ctx.Done():
			return StageResult{}, ctx.Err()
		case <-tick.C:
		}
		state, exists, err = w.inspect(ctx, state.ID)
		if err != nil || !exists {
			return StageResult{}, unknown("generation worker disappeared while observing")
		}
	}
}

func (w *Worker) owns(s containerState, run, input, phase string) bool {
	w.mu.Lock()
	frozen := w.imageID
	w.mu.Unlock()
	return s.Image == frozen && s.Config.Labels["io.lightpath.atg.worker"] == workerLabel && s.Config.Labels["io.lightpath.atg.run"] == runLabel(run) && (input == "" || s.Config.Labels["io.lightpath.atg.input"] == input) && (phase == "" || s.Config.Labels["io.lightpath.atg.phase"] == phase)
}
func neverStarted(s containerState) bool {
	return !s.State.Running && s.State.StartedAt.IsZero() && (s.State.Status == "created" || s.State.Status == "configured")
}
