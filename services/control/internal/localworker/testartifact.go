package localworker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

const MaxTestArtifactBytes = 1_048_576

var javaPathPattern = regexp.MustCompile(`^src/test/java/(?:[A-Za-z_][A-Za-z0-9_]*/)*[A-Za-z_][A-Za-z0-9_]*Test\.java$`)
var testIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+#[A-Za-z_][A-Za-z0-9_]*$`)

type TestFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type TestArtifact struct {
	SchemaVersion   string     `json:"schema_version"`
	Files           []TestFile `json:"files"`
	ExpectedTestIDs []string   `json:"expected_test_ids"`
}

func DecodeTestArtifact(raw []byte) (TestArtifact, error) {
	var a TestArtifact
	if len(raw) == 0 || len(raw) > MaxTestArtifactBytes || !utf8.Valid(raw) {
		return a, fmt.Errorf("test artifact is empty, oversized, or invalid UTF-8")
	}
	if err := strictJSON(raw, &a); err != nil {
		return a, err
	}
	if a.SchemaVersion != "1" || len(a.Files) < 1 || len(a.Files) > 32 || len(a.ExpectedTestIDs) < 1 || len(a.ExpectedTestIDs) > 1000 {
		return a, fmt.Errorf("invalid test artifact version or inventory bounds")
	}
	classes := map[string]bool{}
	seen := map[string]bool{}
	for _, f := range a.Files {
		if len(f.Path) > 240 || !javaPathPattern.MatchString(f.Path) || path.Clean(f.Path) != f.Path || len(f.Content) == 0 || len(f.Content) > 128*1024 || strings.ContainsRune(f.Content, 0) || !strings.HasSuffix(f.Content, "\n") || seen[f.Path] {
			return a, fmt.Errorf("artifact files must be unique new Java *Test.java files with bounded newline-terminated content")
		}
		seen[f.Path] = true
		classes[strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(f.Path, "src/test/java/"), ".java"), "/", ".")] = true
	}
	seen = map[string]bool{}
	for _, id := range a.ExpectedTestIDs {
		if !testIDPattern.MatchString(id) || seen[id] || !classes[strings.Split(id, "#")[0]] {
			return a, fmt.Errorf("expected test IDs must uniquely name admitted classes and methods")
		}
		seen[id] = true
	}
	for class := range classes {
		found := false
		for _, id := range a.ExpectedTestIDs {
			found = found || strings.HasPrefix(id, class+"#")
		}
		if !found {
			return a, fmt.Errorf("every admitted file requires expected test IDs")
		}
	}
	sort.Slice(a.Files, func(i, j int) bool { return a.Files[i].Path < a.Files[j].Path })
	sort.Strings(a.ExpectedTestIDs)
	return a, nil
}

// PatchContent is an applicable, deterministic new-file-only unified patch.
// Expected IDs remain independently bound by the retained raw input artifact.
func (a TestArtifact) PatchContent() []byte {
	var b strings.Builder
	for _, f := range a.Files {
		lines := strings.Split(strings.TrimSuffix(f.Content, "\n"), "\n")
		fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", f.Path, f.Path, f.Path, len(lines))
		for _, line := range lines {
			b.WriteByte('+')
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

func (a TestArtifact) archive() []byte {
	var b bytes.Buffer
	t := tar.NewWriter(&b)
	for _, f := range a.Files {
		_ = t.WriteHeader(&tar.Header{Name: f.Path, Mode: 0600, Typeflag: tar.TypeReg, Size: int64(len(f.Content))})
		_, _ = t.Write([]byte(f.Content))
	}
	_ = t.Close()
	return b.Bytes()
}

func (w *Worker) PrepareTestArtifact(ctx context.Context, r testgenflow.WorkRequest) (testgenflow.GenerationResult, error) {
	if err := w.validateRequest(r); err != nil {
		return testgenflow.GenerationResult{}, err
	}
	digest, err := candidateArtifactDigest(r)
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	raw, err := w.get(ctx, digest, MaxTestArtifactBytes)
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	a, err := DecodeTestArtifact(raw)
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	ref, err := w.put(ctx, raw)
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	patch, err := w.put(ctx, a.PatchContent())
	if err != nil {
		return testgenflow.GenerationResult{}, err
	}
	result := testgenflow.GenerationResult{TestArtifactDigest: ref, PatchDigest: patch}
	for _, f := range a.Files {
		result.ChangedTestPaths = append(result.ChangedTestPaths, f.Path)
	}
	return result, nil
}

func (w *Worker) admitted(ctx context.Context, r testgenflow.WorkRequest) (TestArtifact, error) {
	raw, err := w.get(ctx, r.TestArtifactDigest, MaxTestArtifactBytes)
	if err != nil {
		return TestArtifact{}, err
	}
	a, err := DecodeTestArtifact(raw)
	if err != nil {
		return a, err
	}
	expected, err := candidateArtifactDigest(r)
	if err != nil {
		return a, err
	}
	if artifact.Digest(a.PatchContent()) != r.PatchDigest || r.TestArtifactDigest != expected {
		return a, fmt.Errorf("candidate artifact or canonical patch digest mismatch")
	}
	paths := append([]string(nil), r.ChangedTestPaths...)
	sort.Strings(paths)
	if len(paths) != len(a.Files) {
		return a, fmt.Errorf("candidate paths mismatch")
	}
	for i, f := range a.Files {
		if paths[i] != f.Path {
			return a, fmt.Errorf("candidate paths mismatch")
		}
	}
	return a, nil
}

func candidateArtifactDigest(r testgenflow.WorkRequest) (string, error) {
	switch r.Request.InputMode {
	case testgenflow.InputModeArtifactInput:
		if r.Request.InputArtifactDigest != "" {
			return r.Request.InputArtifactDigest, nil
		}
	case testgenflow.InputModeModelGeneration:
		// The controller supplies this only after recording the code review.
		// This pure materialization boundary does not manufacture an approval.
		if r.Request.Generation != nil && r.Request.InputArtifactDigest == "" && r.Attempt > 0 && r.TestArtifactDigest != "" {
			return r.TestArtifactDigest, nil
		}
	}
	return "", fmt.Errorf("candidate does not have an admitted artifact identity")
}
