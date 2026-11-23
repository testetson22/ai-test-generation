package agentworker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxCollectionBytes = 4 * 1024 * 1024
const receiptMarker = "ATG_GENERATION_RECEIPT="

type Evidence struct {
	SchemaVersion string `json:"schema_version"`
	WorkerID      string `json:"worker_id"`
	RunID         string `json:"run_id"`
	JobID         string `json:"job_id"`
	InputDigest   string `json:"input_digest"`
	PolicyDigest  string `json:"policy_digest"`
	ImageID       string `json:"image_id"`
	Phase         string `json:"phase"`
	TaskRef       string `json:"task_ref,omitempty"`
	ResultRef     string `json:"result_ref,omitempty"`
	LogRef        string `json:"log_ref,omitempty"`
	ExitCode      int    `json:"exit_code"`
	Partial       bool   `json:"partial"`
	InvalidResult bool   `json:"invalid_result"`
	Status        string `json:"status"`
	Diagnostic    string `json:"diagnostic,omitempty"`
}

func collection(raw []byte) (map[string][]byte, error) {
	if len(raw) > maxCollectionBytes {
		return nil, fmt.Errorf("generation collection oversized")
	}
	reader := tar.NewReader(bytes.NewReader(raw))
	files := map[string][]byte{}
	total := int64(0)
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid generation collection")
		}
		name := h.Name
		if h.Typeflag != tar.TypeReg || h.Linkname != "" || files[name] != nil || h.Size < 0 || h.Size > maxTaskBytes || (name != "task.json" && name != "result.json" && name != "command.log" && name != "exit") {
			return nil, fmt.Errorf("unexpected generation collection entry")
		}
		total += h.Size
		if total > maxCollectionBytes {
			return nil, fmt.Errorf("generation collection oversized")
		}
		content, err := io.ReadAll(io.LimitReader(reader, h.Size+1))
		if err != nil || int64(len(content)) != h.Size {
			return nil, fmt.Errorf("truncated generation collection")
		}
		files[name] = content
	}
	return files, nil
}

func (w *Worker) collect(ctx context.Context, s containerState, t Task, taskRaw []byte, partial bool) (StageResult, error) {
	args := []string{"exec", s.ID, "/bin/sh", "/opt/atg/entrypoint.sh", "collect"}
	if partial {
		args = append(args, "partial")
	}
	raw, err := w.exec(ctx, nil, maxCollectionBytes, args...)
	if err != nil {
		return StageResult{}, err
	}
	files, err := collection(raw)
	if err != nil {
		return StageResult{}, err
	}
	// Completion can race a cancellation observation. Prefer the durable wrapper
	// exit record when it appeared before collection, not the older status read.
	if _, finished := files["exit"]; partial && finished {
		partial = false
	}
	// Recovery may use a new adapter instance with no in-memory redaction key.
	// Reload only the explicitly configured private credential, never host env.
	if len(files["command.log"]) > 0 || len(files["result.json"]) > 0 {
		key, err := w.credential()
		if err != nil {
			return StageResult{}, err
		}
		clear(key)
	}
	if len(taskRaw) == 0 && len(files["task.json"]) > 0 {
		taskRaw = files["task.json"]
		if strictJSON(taskRaw, &t) != nil {
			return StageResult{}, fmt.Errorf("invalid retained task")
		}
	}
	if len(files["task.json"]) > 0 && !bytes.Equal(taskRaw, files["task.json"]) {
		return StageResult{}, fmt.Errorf("worker collected a different task")
	}
	input := s.Config.Labels["io.lightpath.atg.input"]
	if len(taskRaw) > 0 && (t.JobID == "" || t.RunID == "" || t.Phase != s.Config.Labels["io.lightpath.atg.phase"] || digestBytes(taskRaw) != input || !w.owns(s, t.RunID, input, t.Phase)) {
		return StageResult{}, fmt.Errorf("collected generation task identity mismatch")
	}
	if w.containsSecret(taskRaw) {
		return StageResult{}, fmt.Errorf("credential contamination in retained task")
	}
	e := Evidence{SchemaVersion: "1", WorkerID: s.ID, RunID: t.RunID, JobID: t.JobID, InputDigest: input, PolicyDigest: w.PolicyDigest(), ImageID: s.Image, Phase: s.Config.Labels["io.lightpath.atg.phase"], Partial: partial, Status: "failed", ExitCode: -1}
	if partial {
		e.Diagnostic = "generation cancelled; partial evidence only"
	}
	if len(taskRaw) > 0 {
		if e.TaskRef, err = w.put(ctx, taskRaw); err != nil {
			return StageResult{}, err
		}
	}
	if log := files["command.log"]; len(log) > 0 {
		log = w.redact(log)
		if w.containsSecret(log) {
			log = []byte("[REDACTED CREDENTIAL DIAGNOSTIC]")
		}
		if e.LogRef, err = w.put(ctx, log); err != nil {
			return StageResult{}, err
		}
	}
	if partial && len(files["result.json"]) > 0 {
		content := w.redact(files["result.json"])
		if w.containsSecret(files["result.json"]) {
			content = []byte(`{"redacted":"credential contamination"}`)
		}
		if len(content) > w.policy.Limits.MaxOutputBytes {
			content = []byte(`{"error":"partial result exceeded policy limit"}`)
		}
		if e.ResultRef, err = w.put(ctx, content); err != nil {
			return StageResult{}, err
		}
	}
	if !partial {
		code, err := strconv.Atoi(strings.TrimSpace(string(files["exit"])))
		if err != nil || code < 0 || code > 255 {
			return StageResult{}, fmt.Errorf("missing generation exit record")
		}
		e.ExitCode = code
		resultRaw := files["result.json"]
		redacted := w.redact(resultRaw)
		r, resultErr := validateResult(resultRaw, taskRaw, t)
		if w.containsSecret(resultRaw) {
			resultErr = fmt.Errorf("credential contamination")
			redacted = []byte(`{"redacted":"credential contamination"}`)
		}
		if len(redacted) == 0 {
			redacted = []byte(`{"error":"generation result missing"}`)
		}
		if len(redacted) > w.policy.Limits.MaxOutputBytes {
			redacted = []byte(`{"error":"generation result exceeded policy limit"}`)
		}
		if resultErr == nil && t.Phase == "code" && r.Status == "completed" {
			var c Code
			_ = strictJSON(r.Output, &c)
			resultErr = w.newTestFiles(ctx, t, c.Files)
		}
		if resultErr != nil || (code != 0 && r.Status == "completed") {
			e.InvalidResult = true
			e.Diagnostic = "generation result failed independent validation"
		} else {
			e.Status = r.Status
			e.Diagnostic = string(w.redact([]byte(r.Diagnostic)))
		}
		if len(redacted) > 0 {
			if e.ResultRef, err = w.put(ctx, redacted); err != nil {
				return StageResult{}, err
			}
		}
	}
	content, _ := json.Marshal(e)
	ref, err := w.put(ctx, content)
	return StageResult{ArtifactRef: ref, Status: e.Status, Diagnostic: e.Diagnostic}, err
}

func (w *Worker) loadEvidence(ctx context.Context, ref string) (Evidence, Task, Result, error) {
	return w.loadEvidenceDepth(ctx, ref, 0)
}

func (w *Worker) loadEvidenceDepth(ctx context.Context, ref string, depth int) (Evidence, Task, Result, error) {
	var e Evidence
	var t Task
	var r Result
	if depth > 2 {
		return e, t, r, fmt.Errorf("generation prior chain exceeds phase bounds")
	}
	raw, err := w.get(ctx, ref, 64*1024)
	if err != nil {
		return e, t, r, err
	}
	if strictJSON(raw, &e) != nil || e.SchemaVersion != "1" || e.PolicyDigest != w.PolicyDigest() || e.WorkerID == "" || !digestPattern.MatchString(e.InputDigest) {
		return e, t, r, fmt.Errorf("invalid generation evidence identity")
	}
	w.mu.Lock()
	frozen := w.imageID
	w.mu.Unlock()
	if e.ImageID != frozen {
		return e, t, r, fmt.Errorf("generation evidence image differs from frozen policy")
	}
	for _, ref := range []string{e.PolicyDigest, w.policy.Digest} {
		if _, err = w.get(ctx, ref, 256*1024); err != nil {
			return e, t, r, err
		}
	}
	if e.LogRef != "" {
		if _, err = w.get(ctx, e.LogRef, maxCollectionBytes); err != nil {
			return e, t, r, err
		}
	}
	if e.TaskRef == "" && e.Partial {
		return e, t, r, nil
	}
	taskRaw, err := w.get(ctx, e.TaskRef, maxTaskBytes)
	if err != nil {
		return e, t, r, err
	}
	if strictJSON(taskRaw, &t) != nil || digestBytes(taskRaw) != e.InputDigest || t.JobID != e.JobID || t.RunID != e.RunID || t.Phase != e.Phase || t.PolicyDigest != e.PolicyDigest || t.Model != w.policy.Model || t.Harness != w.policy.Harness || t.Limits != w.policy.Limits {
		return e, t, r, fmt.Errorf("retained task binding mismatch")
	}
	if err = w.validateTaskSource(ctx, t); err != nil {
		return e, t, r, err
	}
	if err = w.validateTaskPriors(ctx, t, depth); err != nil {
		return e, t, r, err
	}
	if e.Partial {
		if e.ResultRef != "" {
			if _, err = w.get(ctx, e.ResultRef, int64(w.policy.Limits.MaxOutputBytes)); err != nil {
				return e, t, r, err
			}
		}
		return e, t, r, nil
	}
	resultRaw, err := w.get(ctx, e.ResultRef, int64(w.policy.Limits.MaxOutputBytes))
	if err != nil {
		return e, t, r, err
	}
	r, err = validateResult(resultRaw, taskRaw, t)
	if e.InvalidResult {
		if e.Status != "failed" || e.Diagnostic != "generation result failed independent validation" {
			return e, t, r, fmt.Errorf("invalid retained validation failure")
		}
		return e, t, Result{Status: "failed", Diagnostic: e.Diagnostic}, nil
	}
	if err != nil || r.Status != e.Status || (e.ExitCode != 0 && e.Status == "completed") {
		return e, t, r, fmt.Errorf("retained generation result no longer validates")
	}
	if t.Phase == "code" && r.Status == "completed" {
		var c Code
		_ = strictJSON(r.Output, &c)
		if err = w.newTestFiles(ctx, t, c.Files); err != nil {
			return e, t, r, err
		}
	}
	return e, t, r, nil
}

func (w *Worker) validateTaskPriors(ctx context.Context, t Task, depth int) error {
	if (t.Phase == "analyze" && (t.Prior.AnalysisRef != "" || t.Prior.PlanRef != "" || len(t.Prior.Analysis) > 0 || len(t.Prior.Plan) > 0)) || (t.Phase == "plan" && (t.Prior.AnalysisRef == "" || t.Prior.PlanRef != "" || len(t.Prior.Plan) > 0)) || (t.Phase == "code" && (t.Prior.AnalysisRef == "" || t.Prior.PlanRef == "")) {
		return fmt.Errorf("retained phase prior inventory mismatch")
	}
	for _, prior := range []struct {
		ref, phase string
		output     json.RawMessage
	}{{t.Prior.AnalysisRef, "analyze", t.Prior.Analysis}, {t.Prior.PlanRef, "plan", t.Prior.Plan}} {
		if prior.ref == "" {
			continue
		}
		e, p, r, err := w.loadEvidenceDepth(ctx, prior.ref, depth+1)
		if err != nil || e.Partial || r.Status != "completed" || p.Phase != prior.phase || p.RunID != t.RunID || p.Attempt != t.Attempt || p.ProfileDigest != t.ProfileDigest || p.Objective != t.Objective {
			return fmt.Errorf("retained prior artifact binding mismatch")
		}
		want, _ := json.Marshal(p.Source)
		got, _ := json.Marshal(t.Source)
		var a, b bytes.Buffer
		if !bytes.Equal(want, got) || json.Compact(&a, prior.output) != nil || json.Compact(&b, r.Output) != nil || !bytes.Equal(a.Bytes(), b.Bytes()) || (prior.phase == "plan" && p.Prior.AnalysisRef != t.Prior.AnalysisRef) {
			return fmt.Errorf("retained prior content differs from referenced artifact")
		}
	}
	if ref := t.Prior.PreviousCandidateRef; ref != "" {
		// Repair references are identifiers, not embedded executable/model input.
		// Verify the older completed identity without recursively walking repairs.
		raw, err := w.get(ctx, ref, 64*1024)
		if err != nil {
			return err
		}
		var e Evidence
		if strictJSON(raw, &e) != nil || e.RunID != t.RunID || e.Phase != "code" || e.PolicyDigest != t.PolicyDigest || e.Status != "completed" || e.Partial || e.InvalidResult {
			return fmt.Errorf("invalid previous candidate reference")
		}
		raw, err = w.get(ctx, e.TaskRef, maxTaskBytes)
		if err != nil {
			return err
		}
		var previous Task
		if strictJSON(raw, &previous) != nil || previous.Attempt >= t.Attempt || previous.Attempt < 1 || previous.Source.Commit != t.Source.Commit || previous.ProfileDigest != t.ProfileDigest || digestBytes(raw) != e.InputDigest {
			return fmt.Errorf("previous candidate identity mismatch")
		}
	}
	return nil
}

func (w *Worker) validateTaskSource(ctx context.Context, t Task) error {
	if t.SchemaVersion != "1" || (t.Phase != "analyze" && t.Phase != "plan" && t.Phase != "code") || t.Attempt < 1 || t.Attempt > 100 || !text(t.Objective) || !commitPattern.MatchString(t.Source.Commit) || !commitPattern.MatchString(t.Source.TreeID) || !digestPattern.MatchString(t.ProfileDigest) || len(t.Source.Files) != len(w.policy.ContextPaths) {
		return fmt.Errorf("invalid retained generation task")
	}
	p, _, err := w.sourceProfile(ctx, t.ProfileDigest)
	if err != nil || p.RepositoryURL != t.Source.RepositoryURL || p.Commit != t.Source.Commit {
		return fmt.Errorf("retained source/profile binding mismatch")
	}
	archive, err := w.get(ctx, t.Source.ArtifactRef, maxArchiveBytes)
	if err != nil {
		return err
	}
	files, err := contextFiles(archive, w.policy.ContextPaths, w.policy.Limits.MaxContextBytes)
	if err != nil {
		return err
	}
	want, _ := json.Marshal(files)
	got, _ := json.Marshal(t.Source.Files)
	if !bytes.Equal(want, got) {
		return fmt.Errorf("retained context differs from source archive")
	}
	return nil
}

func (w *Worker) receipt(ctx context.Context, s containerState, input string) (Evidence, string, error) {
	raw, err := w.exec(ctx, nil, 4096, "logs", s.ID)
	if err != nil {
		return Evidence{}, "", err
	}
	line := strings.TrimSuffix(string(raw), "\n")
	if !strings.HasPrefix(line, receiptMarker) || strings.ContainsAny(line, "\r\n") {
		return Evidence{}, "", fmt.Errorf("missing unambiguous generation receipt")
	}
	ref := strings.TrimPrefix(line, receiptMarker)
	e, _, _, err := w.loadEvidence(ctx, ref)
	if err != nil || e.WorkerID != s.ID || e.InputDigest != input || !w.owns(s, e.RunID, input, e.Phase) {
		return Evidence{}, "", fmt.Errorf("generation receipt identity mismatch")
	}
	return e, ref, nil
}

func (w *Worker) receiptResult(ctx context.Context, s containerState, input string) (StageResult, error) {
	e, ref, err := w.receipt(ctx, s, input)
	if err != nil || e.Partial || e.ExitCode != s.State.ExitCode {
		return StageResult{}, unknown("completed generation receipt cannot be reconciled")
	}
	return StageResult{ArtifactRef: ref, Status: e.Status, Diagnostic: e.Diagnostic}, nil
}

func (w *Worker) seal(ctx context.Context, s containerState, ref string, cancel bool) error {
	action := "seal"
	if cancel {
		action = "cancel"
	}
	_, _ = w.exec(ctx, []byte(ref+"\n"), 4096, "exec", "-i", s.ID, "/bin/sh", "/opt/atg/entrypoint.sh", action)
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		state, exists, err := w.inspect(ctx, s.ID)
		if err != nil || !exists {
			return unknown("generation termination observation unknown")
		}
		if !state.State.Running {
			e, got, err := w.receipt(ctx, state, state.Config.Labels["io.lightpath.atg.input"])
			if err != nil || got != ref || e.Partial != cancel || (!cancel && state.State.ExitCode != e.ExitCode) {
				return unknown("generation receipt or termination conflicts")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return unknown("generation receipt termination deadline exceeded")
		case <-tick.C:
		}
	}
}

func (w *Worker) containers(ctx context.Context, runID string) ([]containerState, error) {
	if err := w.allowed(); err != nil {
		return nil, err
	}
	if runID == "" || w.PolicyDigest() == "" {
		return nil, fmt.Errorf("run identity and ready policy required")
	}
	raw, err := w.exec(ctx, nil, 128*1024, "ps", "--all", "--filter=label=io.lightpath.atg.worker="+workerLabel, "--filter=label=io.lightpath.atg.run="+runLabel(runID), "--format=json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) > 512 {
		return nil, fmt.Errorf("invalid generation run inventory")
	}
	var states []containerState
	seen := map[string]bool{}
	for _, row := range rows {
		s, exists, err := w.inspect(ctx, row.ID)
		if err != nil || !exists || seen[s.ID] || !w.owns(s, runID, "", "") {
			return nil, unknown("generation run worker missing or foreign")
		}
		seen[s.ID] = true
		states = append(states, s)
	}
	return states, nil
}

func (w *Worker) ConfirmOperations(ctx context.Context, runID string, keys []string) error {
	if err := w.allowed(); err != nil {
		return err
	}
	if runID == "" || w.PolicyDigest() == "" {
		return fmt.Errorf("run identity and ready policy required")
	}
	for _, key := range keys {
		if key == "" {
			return fmt.Errorf("operation key required")
		}
		s, exists, err := w.inspect(ctx, containerName(key))
		if err != nil || !exists || !w.owns(s, runID, "", "") {
			return unknown("journaled generation worker missing or foreign during cancellation")
		}
	}
	return nil
}

func (w *Worker) cancelContainer(ctx context.Context, s containerState, runID string) error {
	if neverStarted(s) {
		return nil
	}
	// Preserve a completed valid result instead of replacing it with a partial
	// cancellation. Cancellation is composed at run level by the controller.
	status, statusErr := w.exec(ctx, nil, 128, "exec", s.ID, "/bin/sh", "/opt/atg/entrypoint.sh", "status")
	partial := statusErr != nil || strings.TrimSpace(string(status)) != "complete"
	t := Task{RunID: runID, JobID: ""}
	result, err := w.collect(ctx, s, t, nil, partial)
	if err == nil {
		retained, _, _, loadErr := w.loadEvidence(ctx, result.ArtifactRef)
		if loadErr != nil {
			err = loadErr
		} else {
			err = w.seal(ctx, s, result.ArtifactRef, retained.Partial)
		}
		if err == nil {
			return nil
		}
	}
	_, _ = w.exec(ctx, nil, 4096, "stop", "--time=5", s.ID)
	state, exists, observeErr := w.inspect(ctx, s.ID)
	if observeErr != nil || !exists || state.State.Running {
		return unknown("generation cancellation termination unknown")
	}
	return unknown("generation stopped without acknowledged retained evidence")
}

func (w *Worker) CancelRun(ctx context.Context, runID string) (string, error) {
	states, err := w.containers(ctx, runID)
	if err != nil {
		return "", err
	}
	if len(states) == 0 {
		return "", unknown("no generation workers available to confirm cancellation")
	}
	var ids []string
	for _, s := range states {
		if s.State.Running {
			if err = w.cancelContainer(ctx, s, runID); err != nil {
				return "", err
			}
		}
		ids = append(ids, s.ID)
	}
	refs, err := w.RunEvidence(ctx, runID)
	if err != nil {
		return "", err
	}
	sort.Strings(ids)
	raw, _ := json.Marshal(struct {
		SchemaVersion        string   `json:"schema_version"`
		RunID                string   `json:"run_id"`
		TerminationConfirmed bool     `json:"termination_confirmed"`
		EvidenceRefs         []string `json:"evidence_refs"`
		WorkerIDs            []string `json:"worker_ids"`
	}{"1", runID, true, refs, ids})
	return w.put(ctx, raw)
}

func (w *Worker) RunEvidence(ctx context.Context, runID string) ([]string, error) {
	states, err := w.containers(ctx, runID)
	if err != nil {
		return nil, err
	}
	refs := []string{}
	for _, s := range states {
		if s.State.Running {
			return nil, fmt.Errorf("generation still active")
		}
		if neverStarted(s) {
			continue
		}
		_, ref, err := w.receipt(ctx, s, s.Config.Labels["io.lightpath.atg.input"])
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, nil
}

func (w *Worker) CleanupRun(ctx context.Context, runID string) error {
	states, err := w.containers(ctx, runID)
	if err != nil {
		return err
	}
	for _, s := range states {
		if s.State.Running {
			return fmt.Errorf("refusing cleanup of active generation")
		}
		if !neverStarted(s) {
			if _, _, err = w.receipt(ctx, s, s.Config.Labels["io.lightpath.atg.input"]); err != nil {
				return err
			}
		}
		_, _ = w.exec(ctx, nil, 4096, "rm", s.ID)
		_, exists, err := w.inspect(ctx, s.ID)
		if err != nil || exists {
			return unknown("generation cleanup not confirmed")
		}
	}
	return nil
}
