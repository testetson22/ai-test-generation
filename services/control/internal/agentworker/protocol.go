package agentworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/localworker"
)

type Prior struct {
	AnalysisRef          string          `json:"analysis_ref,omitempty"`
	Analysis             json.RawMessage `json:"analysis,omitempty"`
	PlanRef              string          `json:"plan_ref,omitempty"`
	Plan                 json.RawMessage `json:"plan,omitempty"`
	PreviousCandidateRef string          `json:"previous_candidate_ref,omitempty"`
	RepairFeedback       string          `json:"repair_feedback,omitempty"`
}

type Task struct {
	SchemaVersion string          `json:"schema_version"`
	JobID         string          `json:"job_id"`
	RunID         string          `json:"run_id"`
	Attempt       int             `json:"attempt"`
	Phase         string          `json:"phase"`
	Objective     string          `json:"objective"`
	ContextInput  json.RawMessage `json:"context_input,omitempty"`
	ProfileDigest string          `json:"profile_digest"`
	PolicyDigest  string          `json:"policy_digest"`
	Harness       string          `json:"harness"`
	Model         Model           `json:"model"`
	Source        Source          `json:"source"`
	Prior         Prior           `json:"prior"`
	Limits        Limits          `json:"limits"`
}

type Usage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
	Requests     *int `json:"requests"`
}

type Result struct {
	SchemaVersion string            `json:"schema_version"`
	JobID         string            `json:"job_id"`
	RunID         string            `json:"run_id"`
	Attempt       int               `json:"attempt"`
	Phase         string            `json:"phase"`
	RequestDigest string            `json:"request_digest"`
	SourceCommit  string            `json:"source_commit"`
	ProfileDigest string            `json:"profile_digest"`
	PolicyDigest  string            `json:"policy_digest"`
	Harness       string            `json:"harness"`
	Model         Model             `json:"model"`
	Status        string            `json:"status"`
	Output        json.RawMessage   `json:"output"`
	Usage         *Usage            `json:"usage"`
	Versions      map[string]string `json:"versions"`
	Diagnostic    string            `json:"diagnostic"`
}

type Citation struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}
type Fact struct {
	Claim     string     `json:"claim"`
	Citations []Citation `json:"citations"`
}
type Analysis struct {
	Summary     string   `json:"summary"`
	Facts       []Fact   `json:"facts"`
	Gaps        []string `json:"gaps"`
	Limitations []string `json:"limitations"`
}
type Scenario struct {
	Name             string            `json:"name"`
	SourcePaths      []string          `json:"source_paths"`
	TestPath         string            `json:"test_path"`
	Behavior         string            `json:"behavior"`
	Oracle           string            `json:"oracle"`
	TestLevel        string            `json:"test_level,omitempty"`
	ExpectedOutcomes map[string]string `json:"expected_outcomes,omitempty"`
}
type Plan struct {
	Summary     string     `json:"summary"`
	Scenarios   []Scenario `json:"scenarios"`
	Limitations []string   `json:"limitations"`
}
type UnimplementedScenario struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}
type Code struct {
	Summary                string                  `json:"summary"`
	Files                  []localworker.TestFile  `json:"files"`
	SuggestedTestIDs       []string                `json:"suggested_test_ids"`
	UnimplementedScenarios []UnimplementedScenario `json:"unimplemented_scenarios,omitempty"`
	Limitations            []string                `json:"limitations"`
}

var testPathPattern = regexp.MustCompile(`^src/test/java/(?:[A-Za-z_][A-Za-z0-9_]*/)*[A-Za-z_][A-Za-z0-9_]*Test\.java$`)
var suggestedIDPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_.$]*#[A-Za-z_$][A-Za-z0-9_$]*$`)

func text(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 16384 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func texts(ss []string) bool {
	if ss == nil || len(ss) > 128 {
		return false
	}
	for _, s := range ss {
		if !text(s) {
			return false
		}
	}
	return true
}

func validateResult(raw, taskRaw []byte, t Task) (Result, error) {
	var r Result
	if len(raw) == 0 || len(raw) > t.Limits.MaxOutputBytes || strictJSON(raw, &r) != nil || !requiredJSON(raw, "schema_version", "job_id", "run_id", "attempt", "phase", "request_digest", "source_commit", "profile_digest", "policy_digest", "harness", "model", "status", "output", "usage", "versions", "diagnostic") {
		return r, fmt.Errorf("invalid bounded generation result JSON")
	}
	if r.SchemaVersion != "1" || r.JobID != t.JobID || r.RunID != t.RunID || r.Attempt != t.Attempt || r.Phase != t.Phase || r.RequestDigest != artifact.Digest(taskRaw) || r.SourceCommit != t.Source.Commit || r.ProfileDigest != t.ProfileDigest || r.PolicyDigest != t.PolicyDigest || r.Harness != t.Harness || r.Model != t.Model {
		return r, fmt.Errorf("generation result identity mismatch")
	}
	if !slices.Contains([]string{"completed", "blocked", "inconclusive", "failed"}, r.Status) || r.Usage == nil || len(r.Versions) < 1 || len(r.Versions) > 16 || len(r.Diagnostic) > 2048 {
		return r, fmt.Errorf("invalid generation result status/metadata")
	}
	var outer map[string]json.RawMessage
	_ = json.Unmarshal(raw, &outer)
	if !requiredJSON(outer["usage"], "input_tokens", "output_tokens", "requests") {
		return r, fmt.Errorf("usage requires explicit nullable counters")
	}
	for key, value := range r.Versions {
		if key == "" || len(key) > 64 || value == "" || len(value) > 128 {
			return r, fmt.Errorf("invalid worker version metadata")
		}
	}
	for _, n := range []*int{r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.Requests} {
		if n != nil && *n < 0 {
			return r, fmt.Errorf("negative usage")
		}
	}
	if (r.Usage.Requests != nil && *r.Usage.Requests > t.Limits.MaxRequests) || (r.Usage.OutputTokens != nil && *r.Usage.OutputTokens > t.Limits.MaxOutputTokens) {
		return r, fmt.Errorf("generation exceeded policy budget")
	}
	if r.Status != "completed" {
		var empty map[string]json.RawMessage
		if strictJSON(r.Output, &empty) != nil || empty == nil || len(empty) != 0 {
			return r, fmt.Errorf("non-completed result must not supply usable output")
		}
		return r, nil
	}
	if err := validateOutput(t, r.Output); err != nil {
		return r, err
	}
	return r, nil
}

func validateOutput(t Task, raw []byte) error {
	files := map[string]SourceFile{}
	for _, f := range t.Source.Files {
		if artifact.Digest([]byte(f.Content)) != f.Digest {
			return fmt.Errorf("source file digest mismatch")
		}
		files[f.Path] = f
	}
	switch t.Phase {
	case "analyze":
		var a Analysis
		if strictJSON(raw, &a) != nil || !text(a.Summary) || a.Facts == nil || len(a.Facts) > 128 || !texts(a.Gaps) || !texts(a.Limitations) {
			return fmt.Errorf("invalid analysis output")
		}
		for _, f := range a.Facts {
			if !text(f.Claim) || len(f.Citations) < 1 || len(f.Citations) > 128 {
				return fmt.Errorf("analysis facts require bounded citations")
			}
			for _, c := range f.Citations {
				src, ok := files[c.Path]
				lines := strings.Count(src.Content, "\n")
				if !strings.HasSuffix(src.Content, "\n") {
					lines++
				}
				if !ok || c.StartLine < 1 || c.EndLine < c.StartLine || c.EndLine > lines {
					return fmt.Errorf("analysis citation outside supplied context")
				}
			}
		}
	case "plan":
		var p Plan
		if strictJSON(raw, &p) != nil || !text(p.Summary) || len(p.Scenarios) < 1 || len(p.Scenarios) > 128 || !texts(p.Limitations) {
			return fmt.Errorf("invalid plan output")
		}
		seen := map[string]bool{}
		for _, s := range p.Scenarios {
			if !text(s.Name) || !text(s.Behavior) || !text(s.Oracle) || !testPathPattern.MatchString(s.TestPath) || len(s.SourcePaths) < 1 || len(s.SourcePaths) > 128 || seen[s.Name] {
				return fmt.Errorf("invalid plan scenario")
			}
			seen[s.Name] = true
			paths := map[string]bool{}
			for _, p := range s.SourcePaths {
				if _, ok := files[p]; !ok || paths[p] {
					return fmt.Errorf("plan references unsupplied or duplicate source")
				}
				paths[p] = true
			}
		}
	case "code":
		var c Code
		if strictJSON(raw, &c) != nil || !text(c.Summary) || len(c.Files) < 1 || len(c.Files) > 32 || c.SuggestedTestIDs == nil || len(c.SuggestedTestIDs) > 128 || !texts(c.Limitations) {
			return fmt.Errorf("invalid code output")
		}
		var p Plan
		if strictJSON(t.Prior.Plan, &p) != nil || len(p.Scenarios) == 0 {
			return fmt.Errorf("code requires validated prior plan")
		}
		allowed := map[string]bool{}
		for _, s := range p.Scenarios {
			allowed[s.TestPath] = true
		}
		seen := map[string]bool{}
		for _, f := range c.Files {
			if !testPathPattern.MatchString(f.Path) || !allowed[f.Path] || seen[f.Path] || len(f.Content) == 0 || len(f.Content) > 128*1024 || strings.ContainsRune(f.Content, 0) || !strings.HasSuffix(f.Content, "\n") {
				return fmt.Errorf("code must contain only unique planned bounded test files")
			}
			seen[f.Path] = true
		}
		seen = map[string]bool{}
		for _, id := range c.SuggestedTestIDs {
			if !suggestedIDPattern.MatchString(id) || seen[id] {
				return fmt.Errorf("invalid suggested test identity")
			}
			seen[id] = true
		}
	default:
		return fmt.Errorf("unsupported generation phase")
	}
	return nil
}

// ReadCodeArtifact supplies only caller-approved identities, never model hints.
// The returned bytes are revalidated by the existing Maven admission boundary.
func (w *Worker) ReadCodeArtifact(ctx context.Context, ref string, independentlyApprovedIDs []string) ([]byte, error) {
	if err := w.allowed(); err != nil {
		return nil, err
	}
	e, t, r, err := w.loadEvidence(ctx, ref)
	if err != nil {
		return nil, err
	}
	if e.Partial || t.Phase != "code" || r.Status != "completed" || len(independentlyApprovedIDs) == 0 {
		return nil, fmt.Errorf("completed reviewed code and independent IDs are required")
	}
	var c Code
	if err := strictJSON(r.Output, &c); err != nil {
		return nil, err
	}
	key, err := w.credential()
	if err != nil {
		return nil, err
	}
	clear(key)
	if w.containsSecret(r.Output) {
		return nil, fmt.Errorf("credential contamination in generated code")
	}
	a := localworker.TestArtifact{SchemaVersion: "1", Files: c.Files, ExpectedTestIDs: append([]string(nil), independentlyApprovedIDs...)}
	raw, _ := json.Marshal(a)
	if _, err := localworker.DecodeTestArtifact(raw); err != nil {
		return nil, err
	}
	if err := w.newTestFiles(ctx, t, c.Files); err != nil {
		return nil, err
	}
	return bytes.Clone(raw), nil
}

func (w *Worker) ApproveCodeArtifact(ctx context.Context, ref string, independentlyApprovedIDs []string) (string, error) {
	raw, err := w.ReadCodeArtifact(ctx, ref, independentlyApprovedIDs)
	if err != nil {
		return "", err
	}
	return w.put(ctx, raw)
}
