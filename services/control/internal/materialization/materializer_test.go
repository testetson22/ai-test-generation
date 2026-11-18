package materialization

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
)

func TestMaterializeApprovedTestsWritesExactBytesAndIsIdempotent(t *testing.T) {
	workspace := t.TempDir()
	request := materializationRequest(workspace)
	result, err := MaterializeApprovedTests(context.Background(), request)
	if err != nil {
		t.Fatalf("materialize tests: %v", err)
	}
	path := filepath.Join(workspace, filepath.FromSlash(request.ChangedFiles[0]))
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "class GeneratedTest {}\n" {
		t.Fatalf("materialized file = %q, %v", content, err)
	}
	if result.PatchDigest != request.ExpectedPatchDigest || result.MaterializedDigest != request.ExpectedTestsDigest || len(result.ChangedFiles) != 1 {
		t.Fatalf("materialization result = %#v", result)
	}
	if _, err := MaterializeApprovedTests(context.Background(), request); err != nil {
		t.Fatalf("idempotent materialization: %v", err)
	}
	if err := VerifyMaterializedTests(workspace, result); err != nil {
		t.Fatalf("verify materialized bytes before execution: %v", err)
	}
	if err := os.WriteFile(path, []byte("mutated after staging"), 0o600); err != nil {
		t.Fatalf("mutate staged test file: %v", err)
	}
	if err := VerifyMaterializedTests(workspace, result); err == nil || !strings.Contains(err.Error(), "digest changed") {
		t.Fatalf("post-staging mutation error = %v", err)
	}
}

func TestMaterializeFromStoreResolvesOnlyApprovedArtifacts(t *testing.T) {
	workspace := t.TempDir()
	request := materializationRequest(workspace)
	store, err := artifact.NewFileStore(t.TempDir(), 1_000_000)
	if err != nil {
		t.Fatalf("open content store: %v", err)
	}
	defer store.Close()
	for _, content := range [][]byte{request.GenerationManifestJSON, request.Patch, request.MaterializedTestsJSON, request.BaseFilesJSON} {
		if _, err := store.Put(context.Background(), content); err != nil {
			t.Fatalf("store approved input artifact: %v", err)
		}
	}
	result, err := MaterializeFromStore(context.Background(), workspace, request.TaskSpec, store, request.Attestor)
	if err != nil {
		t.Fatalf("materialize approved generation from content store: %v", err)
	}
	if result.PatchDigest != request.ExpectedPatchDigest || result.MaterializedDigest != request.ExpectedTestsDigest {
		t.Fatalf("resolved materialization result = %#v", result)
	}
}

func TestMaterializeApprovedTestsRequiresExactPreimage(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "tests", "ExistingTest.java")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create workspace directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("unexpected base\n"), 0o600); err != nil {
		t.Fatalf("write unexpected base: %v", err)
	}
	request := materializationRequest(workspace)
	request.ChangedFiles = []string{"tests/ExistingTest.java"}
	request.Patch = []byte("diff --git a/tests/ExistingTest.java b/tests/ExistingTest.java\n--- a/tests/ExistingTest.java\n+++ b/tests/ExistingTest.java\n@@ -1 +1 @@\n-expected base\n+updated\n")
	request.MaterializedTestsJSON = pythonCanonicalStringMap(map[string]string{request.ChangedFiles[0]: "updated\n"})
	request.BaseFilesJSON = pythonCanonicalStringMap(map[string]string{request.ChangedFiles[0]: "expected base\n"})
	request = bindApprovedRequest(request)
	if _, err := MaterializeApprovedTests(context.Background(), request); err == nil || !strings.Contains(err.Error(), "base content mismatch") {
		t.Fatalf("preimage mismatch error = %v, want base content mismatch", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "unexpected base\n" {
		t.Fatalf("unexpected base after rejection = %q, %v", content, err)
	}
}

func TestMaterializeApprovedTestsRejectsSymlinksFIFOAndTraversal(t *testing.T) {
	t.Run("symlink directory", func(t *testing.T) {
		workspace, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(workspace, "tests")); err != nil {
			t.Fatalf("create symlink: %v", err)
		}
		if _, err := MaterializeApprovedTests(context.Background(), materializationRequest(workspace)); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink directory error = %v", err)
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("outside entries = %v, %v; want empty", entries, err)
		}
	})

	t.Run("fifo target", func(t *testing.T) {
		workspace := t.TempDir()
		filePath := filepath.Join(workspace, "tests", "GeneratedTest.java")
		if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
			t.Fatalf("create workspace directory: %v", err)
		}
		if err := syscall.Mkfifo(filePath, 0o600); err != nil {
			t.Fatalf("create fifo: %v", err)
		}
		if _, err := MaterializeApprovedTests(context.Background(), materializationRequest(workspace)); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("fifo target error = %v", err)
		}
	})

	t.Run("symlink target", func(t *testing.T) {
		workspace, outside := t.TempDir(), filepath.Join(t.TempDir(), "outside.java")
		if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
			t.Fatalf("write outside target: %v", err)
		}
		filePath := filepath.Join(workspace, "tests", "GeneratedTest.java")
		if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
			t.Fatalf("create workspace directory: %v", err)
		}
		if err := os.Symlink(outside, filePath); err != nil {
			t.Fatalf("create target symlink: %v", err)
		}
		if _, err := MaterializeApprovedTests(context.Background(), materializationRequest(workspace)); err == nil {
			t.Fatal("materializer followed or replaced a symlink target")
		}
		content, err := os.ReadFile(outside)
		if err != nil || string(content) != "keep" {
			t.Fatalf("outside target after rejection = %q, %v", content, err)
		}
	})

	t.Run("path traversal", func(t *testing.T) {
		request := materializationRequest(t.TempDir())
		request.ChangedFiles = []string{"tests/../outside.java"}
		request.MaterializedTestsJSON = pythonCanonicalStringMap(map[string]string{request.ChangedFiles[0]: "unsafe"})
		request.Patch = []byte("diff --git a/tests/../outside.java b/tests/../outside.java\n")
		request = bindApprovedRequest(request)
		if _, err := MaterializeApprovedTests(context.Background(), request); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("traversal error = %v", err)
		}
	})
}

func TestMaterializeApprovedTestsRejectsDigestInventoryAndRootMismatches(t *testing.T) {
	request := materializationRequest(t.TempDir())
	request.Patch = append(request.Patch, 'x')
	if _, err := MaterializeApprovedTests(context.Background(), request); err == nil || !strings.Contains(err.Error(), "patch digest mismatch") {
		t.Fatalf("patch digest error = %v", err)
	}

	request = materializationRequest(t.TempDir())
	request.AllowedPaths = []string{"src/test/"}
	request = bindApprovedRequest(request)
	if _, err := MaterializeApprovedTests(context.Background(), request); err == nil || !strings.Contains(err.Error(), "outside approved") {
		t.Fatalf("path root error = %v", err)
	}

	request = materializationRequest(t.TempDir())
	request.ChangedFiles = []string{"tests/OtherTest.java", "tests/AnotherTest.java"}
	request = bindApprovedRequest(request)
	if _, err := MaterializeApprovedTests(context.Background(), request); err == nil || !strings.Contains(err.Error(), "inventory") {
		t.Fatalf("changed file inventory error = %v", err)
	}

	request = materializationRequest(t.TempDir())
	request.Attestor = fixedWorkspaceAttestor{subject: execution.Subject{
		Source:   request.TaskSpec.Subject.Source,
		Identity: execution.Identity{Kind: "source-tree", ID: "different-subject", Version: "1", Digest: "sha256:" + strings.Repeat("1", 64)},
	}}
	if _, err := MaterializeApprovedTests(context.Background(), request); err == nil || !strings.Contains(err.Error(), "attested workspace") {
		t.Fatalf("workspace attestation mismatch error = %v", err)
	}

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create workspace root symlink: %v", err)
	}
	request = materializationRequest(link)
	if _, err := MaterializeApprovedTests(context.Background(), request); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("workspace root symlink error = %v", err)
	}
}

func TestMaterializationRequiresIndependentWorkspaceAttestation(t *testing.T) {
	workspace := t.TempDir()
	request := materializationRequest(workspace)
	request.Attestor = nil
	if _, err := MaterializeApprovedTests(context.Background(), request); err == nil || !strings.Contains(err.Error(), "independent workspace identity attestor") {
		t.Fatalf("missing attestor error = %v", err)
	}
	path := filepath.Join(workspace, filepath.FromSlash(request.ChangedFiles[0]))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("workspace changed without attestation: stat error = %v", err)
	}
}

func TestMaterializeApprovedTestsRequiresPatchToReproduceFinalArtifact(t *testing.T) {
	request := materializationRequest(t.TempDir())
	request.Patch = []byte("diff --git a/tests/GeneratedTest.java b/tests/GeneratedTest.java\nnew file mode 100644\n--- /dev/null\n+++ b/tests/GeneratedTest.java\n@@ -0,0 +1,1 @@\n+class DifferentTest {}\n")
	request = bindApprovedRequest(request)
	if _, err := MaterializeApprovedTests(context.Background(), request); err == nil || !strings.Contains(err.Error(), "does not reproduce approved") {
		t.Fatalf("patch and materialized artifact mismatch = %v", err)
	}
}

func TestMaterializeApprovedTestsPreservesMissingFinalNewline(t *testing.T) {
	workspace := t.TempDir()
	request := materializationRequest(workspace)
	filePath := filepath.Join(workspace, filepath.FromSlash(request.ChangedFiles[0]))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		t.Fatalf("create workspace directory: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write base file: %v", err)
	}
	request.Patch = []byte("diff --git a/tests/GeneratedTest.java b/tests/GeneratedTest.java\n--- a/tests/GeneratedTest.java\n+++ b/tests/GeneratedTest.java\n@@ -1 +1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n")
	request.MaterializedTestsJSON = pythonCanonicalStringMap(map[string]string{request.ChangedFiles[0]: "new"})
	request.BaseFilesJSON = pythonCanonicalStringMap(map[string]string{request.ChangedFiles[0]: "old"})
	request = bindApprovedRequest(request)
	if _, err := MaterializeApprovedTests(context.Background(), request); err != nil {
		t.Fatalf("materialize file without final newline: %v", err)
	}
	content, err := os.ReadFile(filePath)
	if err != nil || string(content) != "new" {
		t.Fatalf("materialized file bytes = %q, %v", content, err)
	}
}

func TestMaterializePythonCanonicalEmptyFileVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/python-empty-files.json")
	if err != nil {
		t.Fatalf("read Python conformance vectors: %v", err)
	}
	var fixture struct {
		Generator string `json:"generator"`
		Vectors   []struct {
			Name               string            `json:"name"`
			BaseFiles          map[string]string `json:"base_files"`
			MaterializedFiles  map[string]string `json:"materialized_files"`
			ChangedFiles       []string          `json:"changed_files"`
			Patch              string            `json:"patch"`
			PatchDigest        string            `json:"patch_digest"`
			MaterializedDigest string            `json:"materialized_digest"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode Python conformance vectors: %v", err)
	}
	if fixture.Generator != "atg.domain.generation.canonical_patch" || len(fixture.Vectors) != 3 {
		t.Fatalf("unexpected conformance fixture: %#v", fixture)
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			request := materializationRequest(t.TempDir())
			request.Patch = []byte(vector.Patch)
			request.ChangedFiles = vector.ChangedFiles
			request.BaseFilesJSON = pythonCanonicalStringMap(vector.BaseFiles)
			request.MaterializedTestsJSON = pythonCanonicalStringMap(vector.MaterializedFiles)
			request = bindApprovedRequest(request)
			if request.ExpectedPatchDigest != vector.PatchDigest || request.ExpectedTestsDigest != vector.MaterializedDigest {
				t.Fatal("approved inputs differ from Python conformance digests")
			}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := MaterializeApprovedTests(context.Background(), request)
				if err != nil {
					t.Fatalf("materialize reference patch, attempt %d: %v", attempt, err)
				}
				if result.PatchDigest != vector.PatchDigest || result.MaterializedDigest != vector.MaterializedDigest || !slices.Equal(result.ChangedFiles, vector.ChangedFiles) {
					t.Fatalf("materialized identity differs from reference: %#v", result)
				}
				for name, expected := range vector.MaterializedFiles {
					filePath := filepath.Join(request.WorkspaceRoot, filepath.FromSlash(name))
					info, err := os.Lstat(filePath)
					if err != nil || !info.Mode().IsRegular() {
						t.Fatalf("materialized target %q is not a regular file: %v", name, err)
					}
					actual, err := os.ReadFile(filePath)
					if err != nil || string(actual) != expected {
						t.Fatalf("materialized %q = %q, %v; want %q", name, actual, err, expected)
					}
				}
				if err := VerifyMaterializedTests(request.WorkspaceRoot, result); err != nil {
					t.Fatalf("verify reference materialization: %v", err)
				}
			}
		})
	}
}

func TestMaterializeEmptyFileRejectsIncompleteOrConflictingCreation(t *testing.T) {
	const name = "tests/__init__.py"
	const header = "diff --git a/tests/__init__.py b/tests/__init__.py\n"
	for _, testCase := range []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "missing creation metadata", mutate: func(r *Request) { r.Patch = []byte(header) }},
		{name: "unsupported creation mode", mutate: func(r *Request) { r.Patch = []byte(header + "new file mode 100755\n") }},
		{name: "nonempty approved file", mutate: func(r *Request) {
			r.MaterializedTestsJSON = pythonCanonicalStringMap(map[string]string{name: "not empty\n"})
		}},
		{name: "empty preimage exists", mutate: func(r *Request) { r.BaseFilesJSON = pythonCanonicalStringMap(map[string]string{name: ""}) }},
		{name: "nonempty preimage exists", mutate: func(r *Request) { r.BaseFilesJSON = pythonCanonicalStringMap(map[string]string{name: "before\n"}) }},
		{name: "unexpected trailing data", mutate: func(r *Request) { r.Patch = append(r.Patch, []byte("unexpected\n")...) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := materializationRequest(t.TempDir())
			request.ChangedFiles = []string{name}
			request.Patch = []byte(header + "new file mode 100644\n")
			request.MaterializedTestsJSON = pythonCanonicalStringMap(map[string]string{name: ""})
			testCase.mutate(&request)
			request = bindApprovedRequest(request)
			if _, err := MaterializeApprovedTests(context.Background(), request); err == nil {
				t.Fatal("invalid header-only creation was accepted")
			}
			entries, err := os.ReadDir(request.WorkspaceRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("workspace changed after rejected creation: %v, %v", entries, err)
			}
		})
	}
}

func TestMaterializeEmptyFileRejectsUnexpectedWorkspaceTarget(t *testing.T) {
	for _, target := range []string{"nonempty", "symlink"} {
		t.Run(target, func(t *testing.T) {
			request := materializationRequest(t.TempDir())
			request.ChangedFiles = []string{"tests/__init__.py"}
			request.Patch = []byte("diff --git a/tests/__init__.py b/tests/__init__.py\nnew file mode 100644\n")
			request.MaterializedTestsJSON = pythonCanonicalStringMap(map[string]string{"tests/__init__.py": ""})
			request = bindApprovedRequest(request)
			filePath := filepath.Join(request.WorkspaceRoot, "tests", "__init__.py")
			if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.py")
			if target == "symlink" {
				if err := os.WriteFile(outside, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filePath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filePath, []byte("keep\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := MaterializeApprovedTests(context.Background(), request); err == nil {
				t.Fatal("empty creation accepted an unexpected target")
			}
			if target == "symlink" {
				actual, err := os.Readlink(filePath)
				if err != nil || actual != outside {
					t.Fatalf("symlink changed after rejection: %q, %v", actual, err)
				}
			} else {
				actual, err := os.ReadFile(filePath)
				if err != nil || string(actual) != "keep\n" {
					t.Fatalf("workspace preimage changed after rejection: %q, %v", actual, err)
				}
			}
		})
	}
}

func TestPythonCanonicalMaterializedDigestIsStableForUnicode(t *testing.T) {
	files := map[string]string{"tests/test_unicode.py": "# separator \u2028\nassert value == 1\n"}
	if got := digest(pythonCanonicalStringMap(files)); got != "sha256:0c4441d38540cdff3f5b67be356282eb288efff3d39c2bae67fd7663e4c2ed89" {
		t.Fatalf("canonical materialized digest = %s", got)
	}
}

func materializationRequest(workspace string) Request {
	files := map[string]string{"tests/GeneratedTest.java": "class GeneratedTest {}\n"}
	patch := []byte("diff --git a/tests/GeneratedTest.java b/tests/GeneratedTest.java\nnew file mode 100644\n--- /dev/null\n+++ b/tests/GeneratedTest.java\n@@ -0,0 +1,1 @@\n+class GeneratedTest {}\n")
	testsJSON := pythonCanonicalStringMap(files)
	baseJSON := []byte("{}")
	request := Request{
		WorkspaceRoot: workspace,
		Attestor:      echoWorkspaceAttestor{},
		Patch:         patch, MaterializedTestsJSON: testsJSON, BaseFilesJSON: baseJSON,
		AllowedPaths: []string{"tests/"}, ChangedFiles: []string{"tests/GeneratedTest.java"},
	}
	return bindApprovedRequest(request)
}

func bindApprovedRequest(request Request) Request {
	files, err := decodeStringMap(request.MaterializedTestsJSON, "tests")
	if err != nil {
		panic(err)
	}
	baseFiles, err := decodeStringMap(request.BaseFilesJSON, "base files")
	if err != nil {
		panic(err)
	}
	patchDigest := digest(request.Patch)
	testsDigest := digest(pythonCanonicalStringMap(files))
	baseDigest := digest(pythonCanonicalStringMap(baseFiles))
	base := execution.Identity{Kind: "source-tree", ID: "subject-1", Version: "1", Digest: "sha256:" + strings.Repeat("1", 64)}
	candidate := execution.Identity{Kind: "source-tree", ID: "candidate-1", Version: "2", Digest: "sha256:" + strings.Repeat("2", 64)}
	patchRef := execution.Artifact{ArtifactID: "patch-1", MediaType: "text/x-diff", Digest: patchDigest, SizeBytes: int64(len(request.Patch))}
	testsCanonical := pythonCanonicalStringMap(files)
	testsRef := execution.Artifact{ArtifactID: "tests-1", MediaType: "application/vnd.atg.materialized-tests+json", Digest: testsDigest, SizeBytes: int64(len(testsCanonical))}
	baseCanonical := pythonCanonicalStringMap(baseFiles)
	baseRef := execution.Artifact{ArtifactID: "base-1", MediaType: "application/vnd.atg.base-files+json", Digest: baseDigest, SizeBytes: int64(len(baseCanonical))}
	subjects := []generationSubjectMaterialization{{Subject: base, TestDigest: testsDigest}, {Subject: candidate, TestDigest: testsDigest}}
	manifest := generationManifest{
		Contract: "atg.generation", Version: "1.0", GenerationID: "generation-1", RunID: "run-1",
		PlanID: "plan-1", PlanRevision: 1, PlanArtifactDigest: "sha256:" + strings.Repeat("e", 64),
		PlanCheckpointID: "plan-checkpoint-1", PlanDecisionID: "plan-decision-1", Base: base, Candidate: candidate,
		PatchDigest: patchDigest, MaterializedTestDigest: testsDigest, SubjectMaterializations: subjects,
		ChangedFiles: append([]string(nil), request.ChangedFiles...), Status: "verified",
		Attempts: []generationAttempt{{
			PatchArtifactID: patchRef.ArtifactID, MaterializedArtifactID: testsRef.ArtifactID, BaseArtifactID: baseRef.ArtifactID,
			OutputDigest: patchDigest, ChangedFiles: append([]string(nil), request.ChangedFiles...), Status: "verified",
		}},
		Artifacts: []execution.Artifact{patchRef, testsRef, baseRef},
		Handoff: generationHandoff{
			Status: "ready", GenerationID: "generation-1", RunID: "run-1", PlanID: "plan-1", PlanRevision: 1,
			PlanArtifactDigest: "sha256:" + strings.Repeat("e", 64), PlanCheckpointID: "plan-checkpoint-1", PlanDecisionID: "plan-decision-1",
			Subjects: subjects, AllowedPaths: append([]string(nil), request.AllowedPaths...), ChangedFiles: append([]string(nil), request.ChangedFiles...),
			PatchDigest: patchDigest, MaterializedTestDigest: testsDigest, PatchArtifactID: patchRef.ArtifactID,
			MaterializedArtifactID: testsRef.ArtifactID, BaseArtifactID: baseRef.ArtifactID,
		},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	manifestDigest := digest(manifestJSON)
	reviewer := execution.Reviewer{Identity: execution.Identity{Kind: "person", ID: "reviewer-1"}, Role: "test-reviewer", AuthorizationRef: "authz-1"}
	profileDigest := "sha256:" + strings.Repeat("7", 64)
	request.TaskSpec = execution.TaskSpec{
		Contract: "atg.execution_task_spec", Version: "1.0", TaskSpecID: "task-spec-1", RunID: "run-1",
		ExpectedRunRevision: 1, PairID: "pair-1", GenerationID: "generation-1", SubjectRole: "base", ComparisonMode: "same_subject_control",
		Subject: execution.Subject{
			Source:   execution.Source{Identity: execution.Identity{Kind: "git-commit", ID: "commit-1", Digest: "sha256:" + strings.Repeat("f", 64)}, Locator: "https://example.invalid/repo.git"},
			Identity: base,
		},
		Generation: execution.GenerationRefs{
			Manifest: execution.Artifact{ArtifactID: "manifest-1", MediaType: "application/vnd.atg.generation+json", Digest: manifestDigest, SizeBytes: int64(len(manifestJSON))},
			Patch:    patchRef, Tests: testsRef,
		},
		CodeApproval:    execution.CodeApproval{CheckpointID: "code-checkpoint-1", DecisionID: "code-decision-1", Reviewer: reviewer, ReviewedGenerationDigest: manifestDigest},
		Profile:         execution.ProfileRef{ProfileID: "spring-profile-1", ProfileVersion: "1.0.0", Artifact: execution.Artifact{ArtifactID: "profile-1", MediaType: "application/vnd.atg.execution-profile+json", Digest: profileDigest}},
		ProfileApproval: execution.ProfileApproval{DecisionID: "profile-decision-1", Reviewer: reviewer, ApprovedProfileDigest: profileDigest},
		ExpectedTestIDs: []string{"fixture.SmokeTest#works"},
		RetryPolicy:     execution.RetryPolicy{PolicyID: "retry-1", MaxAttempts: 2, DeadlineAt: "2026-11-18T19:00:00Z", InitialBackoffSeconds: 1, MaxBackoffSeconds: 30, RetryableFailures: []string{"worker_lost"}},
		CreatedAt:       "2026-11-17T19:00:00Z",
	}
	request.GenerationManifestJSON = manifestJSON
	request.ExpectedPatchDigest = patchDigest
	request.ExpectedTestsDigest = testsDigest
	request.ExpectedBaseFilesDigest = baseDigest
	if err := request.TaskSpec.Validate(); err != nil {
		panic(err)
	}
	return request
}

type echoWorkspaceAttestor struct{}

func (echoWorkspaceAttestor) AttestWorkspace(_ context.Context, _ string, expected execution.Subject) (execution.Subject, error) {
	return expected, nil
}

type fixedWorkspaceAttestor struct{ subject execution.Subject }

func (attestor fixedWorkspaceAttestor) AttestWorkspace(context.Context, string, execution.Subject) (execution.Subject, error) {
	return attestor.subject, nil
}
