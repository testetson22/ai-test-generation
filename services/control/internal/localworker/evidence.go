package localworker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/report"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

const MaxEvidenceBytes int64 = 40 * 1024 * 1024

type Evidence struct {
	SchemaVersion      string            `json:"schema_version"`
	WorkerID           string            `json:"worker_id"`
	InputDigest        string            `json:"input_digest"`
	ProfileDigest      string            `json:"profile_digest"`
	ImageID            string            `json:"image_id"`
	SourceCommit       string            `json:"source_commit"`
	Subject            string            `json:"subject,omitempty"`
	Phase              string            `json:"phase"`
	PatchDigest        string            `json:"patch_digest,omitempty"`
	TestArtifactDigest string            `json:"test_artifact_digest,omitempty"`
	ExitCode           int               `json:"exit_code"`
	Partial            bool              `json:"partial"`
	ExpectedTestIDs    []string          `json:"expected_test_ids,omitempty"`
	Summary            report.Summary    `json:"summary"`
	Artifacts          map[string]string `json:"artifacts"`
	Succeeded          bool              `json:"succeeded"`
	Diagnostic         string            `json:"diagnostic,omitempty"`
}

func (w *Worker) put(ctx context.Context, raw []byte) (string, error) {
	ref, err := w.store.Put(ctx, raw)
	if err != nil {
		return "", err
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

func (w *Worker) get(ctx context.Context, digest string, limit int64) ([]byte, error) {
	raw, err := w.store.Get(ctx, digest, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, artifact.ErrTooLarge
	}
	if artifact.Digest(raw) != digest {
		return nil, artifact.ErrCorrupt
	}
	return raw, nil
}

func decodeCollection(raw []byte) (map[string][]byte, error) {
	if int64(len(raw)) > MaxEvidenceBytes {
		return nil, artifact.ErrTooLarge
	}
	t := tar.NewReader(bytes.NewReader(raw))
	files := map[string][]byte{}
	total := int64(0)
	for {
		h, err := t.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir && (name == "" || name == "." || name == "reports" || name == "reports/") {
			continue
		}
		if h.Typeflag != tar.TypeReg || path.Clean(name) != name || files[name] != nil || len(files) >= 128 || h.Size < 0 || h.Size > 32*1024*1024 {
			return nil, fmt.Errorf("invalid or oversized worker evidence entry")
		}
		if name != "exit" && name != "source-identity" && name != "source.tar.gz" && name != "toolchain" && name != "command.log" && !(strings.HasPrefix(name, "reports/TEST-") && strings.HasSuffix(name, ".xml") && path.Dir(name) == "reports") {
			return nil, fmt.Errorf("unexpected worker evidence path")
		}
		total += h.Size
		if total > MaxEvidenceBytes {
			return nil, artifact.ErrTooLarge
		}
		content, err := io.ReadAll(io.LimitReader(t, h.Size+1))
		if err != nil || int64(len(content)) != h.Size {
			return nil, fmt.Errorf("truncated worker evidence")
		}
		files[name] = content
	}
	return files, nil
}

func summarize(files map[string][]byte, expected []string) (report.Summary, error) {
	combined := report.Summary{MediaType: report.JUnitMediaType}
	seen := map[string]bool{}
	expectedSet := map[string]bool{}
	for _, id := range expected {
		expectedSet[id] = true
	}
	total := 0
	count := 0
	var reportNames []string
	for name := range files {
		if strings.HasPrefix(name, "reports/") {
			reportNames = append(reportNames, name)
		}
	}
	sort.Strings(reportNames)
	for _, name := range reportNames {
		raw := files[name]
		total += len(raw)
		count++
		if total > report.MaxReportBytes {
			return combined, report.ErrReportTooLarge
		}
		s, err := report.Parse(report.JUnitMediaType, raw, expected, report.MaxReportBytes)
		if err != nil {
			return combined, err
		}
		for _, id := range s.ReportedTestIDs {
			if seen[id] {
				return combined, fmt.Errorf("duplicate test identity across reports")
			}
			seen[id] = true
			combined.ReportedTestIDs = append(combined.ReportedTestIDs, id)
			if !expectedSet[id] {
				combined.UnexpectedTestIDs = append(combined.UnexpectedTestIDs, id)
			}
		}
		combined.ExecutedTestIDs = append(combined.ExecutedTestIDs, s.ExecutedTestIDs...)
		combined.SkippedTestIDs = append(combined.SkippedTestIDs, s.SkippedTestIDs...)
		combined.FailedTestIDs = append(combined.FailedTestIDs, s.FailedTestIDs...)
	}
	for _, id := range expected {
		if !seen[id] {
			combined.MissingTestIDs = append(combined.MissingTestIDs, id)
		}
	}
	for _, ids := range [][]string{combined.ReportedTestIDs, combined.ExecutedTestIDs, combined.SkippedTestIDs, combined.FailedTestIDs, combined.MissingTestIDs, combined.UnexpectedTestIDs} {
		sort.Strings(ids)
	}
	combined.EvidenceComplete = count > 0 && len(expected) > 0 && len(combined.MissingTestIDs) == 0 && len(combined.UnexpectedTestIDs) == 0
	if count == 0 {
		return combined, fmt.Errorf("no fresh JUnit XML reports collected")
	}
	return combined, nil
}

func (w *Worker) collect(ctx context.Context, state containerState, input, phase string, code int, a TestArtifact, testArtifactDigest string, partial bool, subject executionSubject) (testgenflow.MavenResult, error) {
	args := []string{"exec", state.ID, "/bin/bash", "/opt/atg/worker.sh", "collect"}
	if partial {
		args = append(args, "partial")
	}
	raw, err := w.exec(ctx, nil, MaxEvidenceBytes, args...)
	if err != nil {
		return testgenflow.MavenResult{}, err
	}
	files, err := decodeCollection(raw)
	if err != nil {
		return testgenflow.MavenResult{}, err
	}
	e := Evidence{SchemaVersion: "1", WorkerID: state.ID, InputDigest: input, ProfileDigest: w.ProfileDigest(), ImageID: state.Image, SourceCommit: subject.commit, Subject: subject.name, Phase: phase, ExitCode: code, Partial: partial, Artifacts: map[string]string{}}
	if len(a.Files) > 0 {
		e.PatchDigest = artifact.Digest(a.PatchContent())
		e.TestArtifactDigest = testArtifactDigest
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ref, err := w.put(ctx, files[name])
		if err != nil {
			return testgenflow.MavenResult{}, err
		}
		e.Artifacts[name] = ref
	}
	validSource := strings.HasPrefix(string(files["source-identity"]), subject.commit+"\n") && len(files["source.tar.gz"]) > 0 && len(files["toolchain"]) > 0
	actualCode, codeErr := strconv.Atoi(strings.TrimSpace(string(files["exit"])))
	if !partial && (codeErr != nil || actualCode != code) {
		return testgenflow.MavenResult{}, fmt.Errorf("worker exit record missing or inconsistent")
	}
	e.Succeeded = !partial && code == 0 && validSource
	if phase != "source" {
		e.ExpectedTestIDs = w.profile.ExpectedTestIDs()
		if phase == "focused_tests" {
			e.ExpectedTestIDs = append([]string(nil), a.ExpectedTestIDs...)
		} else if phase == "verify" {
			e.ExpectedTestIDs = append(e.ExpectedTestIDs, a.ExpectedTestIDs...)
			sort.Strings(e.ExpectedTestIDs)
		}
		e.Summary, err = summarize(files, e.ExpectedTestIDs)
		minimum := len(w.profile.ExpectedTestIDs())
		if phase == "focused_tests" {
			minimum = len(a.ExpectedTestIDs)
		} else if phase == "verify" {
			minimum += len(a.ExpectedTestIDs)
		}
		e.Succeeded = e.Succeeded && err == nil && e.Summary.EvidenceComplete && len(e.Summary.ExecutedTestIDs) >= minimum && len(e.Summary.SkippedTestIDs) == 0 && len(e.Summary.FailedTestIDs) == 0
	}
	if !e.Succeeded {
		e.Diagnostic = "worker failed or test/source evidence is incomplete"
		if partial {
			e.Diagnostic = "worker cancelled; partial evidence only"
		}
	}
	content, marshalErr := json.Marshal(e)
	if marshalErr != nil {
		return testgenflow.MavenResult{}, marshalErr
	}
	ref, err := w.put(ctx, content)
	if err != nil {
		return testgenflow.MavenResult{}, err
	}
	return testgenflow.MavenResult{Succeeded: e.Succeeded, EvidenceRef: ref, Diagnostic: e.Diagnostic}, nil
}

func (w *Worker) seal(ctx context.Context, id, ref string, cancel bool) error {
	action := "seal"
	if cancel {
		action = "cancel"
	}
	_, commandErr := w.exec(ctx, []byte(ref+"\n"), 4096, "exec", "-i", id, "/bin/bash", "/opt/atg/worker.sh", action)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		state, exists, err := w.inspect(ctx, id)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("worker disappeared before termination confirmation")
		}
		if !state.State.Running {
			e, got, err := w.receipt(ctx, state, state.Config.Labels["io.lightpath.atg.input"])
			if err != nil {
				return errors.Join(commandErr, err)
			}
			if got != ref {
				return fmt.Errorf("worker sealed a conflicting receipt")
			}
			if e.Partial != cancel || (!cancel && e.ExitCode != state.State.ExitCode) {
				return fmt.Errorf("engine exit outcome conflicts with retained worker evidence")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.Join(commandErr, w.stop(ctx, id), fmt.Errorf("worker did not acknowledge its retained evidence receipt"))
		case <-tick.C:
		}
	}
}

func (w *Worker) receipt(ctx context.Context, state containerState, input string) (Evidence, string, error) {
	raw, err := w.exec(ctx, nil, 1024*1024, "logs", state.ID)
	if err != nil {
		return Evidence{}, "", err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "ATG_RECEIPT=sha256:") {
		return Evidence{}, "", fmt.Errorf("worker has no unambiguous retained evidence receipt")
	}
	ref := strings.TrimPrefix(lines[0], "ATG_RECEIPT=")
	content, err := w.get(ctx, ref, MaxEvidenceBytes)
	if err != nil {
		return Evidence{}, "", err
	}
	var e Evidence
	if err = strictJSON(content, &e); err != nil {
		return e, "", err
	}
	if e.SchemaVersion != "1" || e.WorkerID != state.ID || e.InputDigest != input || e.ProfileDigest != w.ProfileDigest() || e.ImageID != state.Image || !w.validSubjectEvidence(e) || (len(e.Artifacts) == 0 && !e.Partial) {
		return e, "", fmt.Errorf("retained worker receipt identity mismatch")
	}
	for _, digest := range e.Artifacts {
		if _, err = w.get(ctx, digest, MaxEvidenceBytes); err != nil {
			return e, "", err
		}
	}
	for _, digest := range []string{e.ProfileDigest, w.profile.Digest, e.PatchDigest, e.TestArtifactDigest} {
		if digest != "" {
			if _, err = w.get(ctx, digest, MaxEvidenceBytes); err != nil {
				return e, "", err
			}
		}
	}
	return e, ref, nil
}

func (w *Worker) receiptResult(ctx context.Context, state containerState, input string) (testgenflow.MavenResult, error) {
	e, ref, err := w.receipt(ctx, state, input)
	if err != nil {
		return testgenflow.MavenResult{}, unknown("completed worker evidence cannot be reconciled", err)
	}
	if e.Partial || e.ExitCode != state.State.ExitCode {
		return testgenflow.MavenResult{}, unknown("worker was cancelled or exit identity conflicts", nil)
	}
	return testgenflow.MavenResult{Succeeded: e.Succeeded, EvidenceRef: ref, Diagnostic: e.Diagnostic}, nil
}

func (w *Worker) containers(ctx context.Context, runID string) ([]containerState, error) {
	if runID == "" {
		return nil, fmt.Errorf("run identity required")
	}
	raw, err := w.exec(ctx, nil, 128*1024, "ps", "--all", "--filter=label=io.lightpath.atg.worker="+w.profile.WorkerRecipe, "--filter=label=io.lightpath.atg.run="+runLabel(runID), "--format=json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID string `json:"Id"`
	}
	if err = json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	var states []containerState
	for _, row := range rows {
		state, exists, err := w.inspect(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, unknown("run worker disappeared", nil)
		}
		if state.Config.Labels["io.lightpath.atg.worker"] != w.profile.WorkerRecipe || state.Config.Labels["io.lightpath.atg.run"] != runLabel(runID) {
			return nil, fmt.Errorf("run worker ownership mismatch")
		}
		states = append(states, state)
	}
	return states, nil
}

// ConfirmOperations binds cancellation to the controller's durable inventory.
// Engine label listing alone cannot reveal a missing journaled operation.
func (w *Worker) ConfirmOperations(ctx context.Context, runID string, keys []string) error {
	if runID == "" {
		return fmt.Errorf("run identity required")
	}
	for _, key := range keys {
		if key == "" {
			return fmt.Errorf("operation key required")
		}
		names := []string{containerName(key)}
		if w.profile.ExecutionPair != nil {
			names = []string{containerName(key + ":base"), containerName(key + ":candidate")}
		}
		confirmed := false
		for _, name := range names {
			state, exists, err := w.inspect(ctx, name)
			if err != nil {
				return unknown("journaled worker is missing during cancellation", err)
			}
			if exists && state.Config.Labels["io.lightpath.atg.worker"] == w.profile.WorkerRecipe && state.Config.Labels["io.lightpath.atg.run"] == runLabel(runID) {
				confirmed = true
			}
		}
		if !confirmed {
			return unknown("journaled worker is missing during cancellation", nil)
		}
	}
	return nil
}

// validSubjectEvidence binds retained evidence to the profile's immutable
// subject set: single-subject profiles pin the profile commit with no subject
// name; pair profiles require a named subject matching its pinned commit.
func (w *Worker) validSubjectEvidence(e Evidence) bool {
	if w.profile.ExecutionPair == nil {
		return e.Subject == "" && e.SourceCommit == w.profile.Commit
	}
	switch e.Subject {
	case "base":
		return e.SourceCommit == w.profile.ExecutionPair.BaseCommit
	case "candidate":
		return e.SourceCommit == w.profile.ExecutionPair.CandidateCommit
	}
	return false
}

// subjectFromState reconstructs a container's execution subject from its label.
func (w *Worker) subjectFromState(state containerState) executionSubject {
	if w.profile.ExecutionPair != nil {
		switch state.Config.Labels["io.lightpath.atg.subject"] {
		case "base":
			return executionSubject{name: "base", commit: w.profile.ExecutionPair.BaseCommit}
		case "candidate":
			return executionSubject{name: "candidate", commit: w.profile.ExecutionPair.CandidateCommit}
		}
		return w.profile.primarySubject()
	}
	return executionSubject{name: "", commit: w.profile.Commit}
}

func (w *Worker) cancelContainer(ctx context.Context, state containerState, input string) error {
	result, collectErr := w.collect(ctx, state, input, "cancelled", -1, TestArtifact{}, "", true, w.subjectFromState(state))
	if collectErr == nil {
		if err := w.seal(ctx, state.ID, result.EvidenceRef, true); err == nil {
			return nil
		}
	}
	// Termination takes priority over evidence when retention fails. Report the
	// evidence loss explicitly; never claim successful cleanup in this case.
	stopErr := w.stop(ctx, state.ID)
	return errors.Join(collectErr, stopErr, fmt.Errorf("worker cancelled without an acknowledged evidence receipt"))
}

func (w *Worker) CancelRun(ctx context.Context, runID string) (string, error) {
	states, err := w.containers(ctx, runID)
	if err != nil {
		return "", unknown("worker cancellation observation failed", err)
	}
	if len(states) == 0 {
		return "", unknown("no run workers available to confirm cancellation", nil)
	}
	var failures []error
	for _, state := range states {
		if state.State.Running {
			if err = w.cancelContainer(ctx, state, state.Config.Labels["io.lightpath.atg.input"]); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err = errors.Join(failures...); err != nil {
		return "", unknown("worker cancellation or evidence retention is unconfirmed", err)
	}
	refs, err := w.RunEvidence(ctx, runID)
	if err != nil {
		return "", unknown("worker cancellation receipt is unavailable", err)
	}
	// This aggregate is written only after independent engine termination checks.
	var ids []string
	for _, state := range states {
		ids = append(ids, state.ID)
	}
	sort.Strings(ids)
	content, err := json.Marshal(struct {
		SchemaVersion        string   `json:"schema_version"`
		RunID                string   `json:"run_id"`
		TerminationConfirmed bool     `json:"termination_confirmed"`
		EvidenceRefs         []string `json:"evidence_refs"`
		WorkerIDs            []string `json:"worker_ids"`
	}{"1", runID, true, refs, ids})
	if err != nil {
		return "", err
	}
	ref, err := w.put(ctx, content)
	if err != nil {
		return "", unknown("worker cancellation manifest retention failed", err)
	}
	return ref, nil
}

// RunEvidence returns verified operation manifests, including source/toolchain
// and cancelled partial evidence, for a final artifact publisher.
func (w *Worker) RunEvidence(ctx context.Context, runID string) ([]string, error) {
	states, err := w.containers(ctx, runID)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, state := range states {
		if state.State.Running {
			return nil, fmt.Errorf("run still has active workers")
		}
		if neverStarted(state) {
			continue
		}
		_, ref, err := w.receipt(ctx, state, state.Config.Labels["io.lightpath.atg.input"])
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
	for _, state := range states {
		if state.State.Running {
			return fmt.Errorf("refusing cleanup before termination")
		}
		if !neverStarted(state) {
			if _, _, err = w.receipt(ctx, state, state.Config.Labels["io.lightpath.atg.input"]); err != nil {
				return err
			}
		}
		_, removeErr := w.exec(ctx, nil, 4096, "rm", state.ID)
		_, exists, err := w.inspect(ctx, state.ID)
		if err != nil {
			return err
		}
		if exists {
			return errors.Join(removeErr, fmt.Errorf("worker deletion not confirmed"))
		}
	}
	return nil
}

func neverStarted(state containerState) bool {
	return !state.State.Running && state.State.StartedAt.IsZero() && (state.State.Status == "created" || state.State.Status == "configured")
}
