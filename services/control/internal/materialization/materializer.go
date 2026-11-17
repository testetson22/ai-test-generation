package materialization

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
)

const (
	MaxPatchBytes = 5_000_000
	MaxFileBytes  = 1_000_000
	MaxTotalBytes = 5_000_000
	MaxFileCount  = 10
	MaxJSONBytes  = 6_000_000
	MaxPathRoots  = 64
)

var supportedPath = regexp.MustCompile(`^[A-Za-z0-9._/-]+\.(java|py|kt|ts|js|go)$`)
var unifiedHunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?: .*)?$`)

type Request struct {
	WorkspaceRoot           string
	Subject                 execution.Identity
	WorkspaceSubject        execution.Identity
	TaskSpec                execution.TaskSpec
	GenerationManifestJSON  []byte
	Patch                   []byte
	MaterializedTestsJSON   []byte
	BaseFilesJSON           []byte
	AllowedPaths            []string
	ChangedFiles            []string
	ExpectedPatchDigest     string
	ExpectedTestsDigest     string
	ExpectedBaseFilesDigest string
}

type generationManifest struct {
	Contract                string                             `json:"contract"`
	Version                 string                             `json:"version"`
	GenerationID            string                             `json:"generation_id"`
	RunID                   string                             `json:"run_id"`
	PlanID                  string                             `json:"plan_id"`
	PlanRevision            int                                `json:"plan_revision"`
	PlanArtifactDigest      string                             `json:"plan_artifact_digest"`
	PlanCheckpointID        string                             `json:"plan_checkpoint_id"`
	PlanDecisionID          string                             `json:"plan_decision_id"`
	Base                    execution.Identity                 `json:"base"`
	Candidate               execution.Identity                 `json:"candidate"`
	PatchDigest             string                             `json:"patch_digest"`
	MaterializedTestDigest  string                             `json:"materialized_test_digest"`
	SubjectMaterializations []generationSubjectMaterialization `json:"subject_materializations"`
	ChangedFiles            []string                           `json:"changed_files"`
	Status                  string                             `json:"status"`
	Attempts                []generationAttempt                `json:"attempts"`
	Artifacts               []execution.Artifact               `json:"artifacts"`
	Handoff                 generationHandoff                  `json:"materialization_handoff"`
}

type generationSubjectMaterialization struct {
	Subject    execution.Identity `json:"subject"`
	TestDigest string             `json:"test_digest"`
}

type generationAttempt struct {
	PatchArtifactID        string   `json:"patch_artifact_id"`
	MaterializedArtifactID string   `json:"materialized_artifact_id"`
	BaseArtifactID         string   `json:"base_artifact_id"`
	OutputDigest           string   `json:"output_digest"`
	ChangedFiles           []string `json:"changed_files"`
	Status                 string   `json:"status"`
}

type generationHandoff struct {
	Status                 string                             `json:"status"`
	GenerationID           string                             `json:"generation_id"`
	RunID                  string                             `json:"run_id"`
	PlanID                 string                             `json:"plan_id"`
	PlanRevision           int                                `json:"plan_revision"`
	PlanArtifactDigest     string                             `json:"plan_artifact_digest"`
	PlanCheckpointID       string                             `json:"plan_checkpoint_id"`
	PlanDecisionID         string                             `json:"plan_decision_id"`
	Subjects               []generationSubjectMaterialization `json:"subjects"`
	AllowedPaths           []string                           `json:"allowed_paths"`
	ChangedFiles           []string                           `json:"changed_files"`
	PatchDigest            string                             `json:"patch_digest"`
	MaterializedTestDigest string                             `json:"materialized_test_digest"`
	PatchArtifactID        string                             `json:"patch_artifact_id"`
	MaterializedArtifactID string                             `json:"materialized_artifact_id"`
	BaseArtifactID         string                             `json:"base_artifact_id"`
}

type Result struct {
	Subject            execution.Identity
	ChangedFiles       []string
	AllowedPaths       []string
	PatchDigest        string
	MaterializedDigest string
	BaseFilesDigest    string
}

// MaterializeApprovedTests verifies the resolved approved handoff, content
// digests, and exact workspace preimages before writing generated test files.
func MaterializeApprovedTests(request Request) (Result, error) {
	approved, err := verifyApprovedHandoff(request)
	if err != nil {
		return Result{}, err
	}
	request.Subject = request.TaskSpec.Subject.Identity
	request.AllowedPaths = approved.AllowedPaths
	request.ChangedFiles = approved.ChangedFiles
	request.ExpectedPatchDigest = approved.PatchDigest
	request.ExpectedTestsDigest = approved.MaterializedTestDigest
	request.ExpectedBaseFilesDigest = approved.BaseFilesDigest
	if len(request.MaterializedTestsJSON) == 0 || len(request.MaterializedTestsJSON) > MaxJSONBytes || len(request.BaseFilesJSON) == 0 || len(request.BaseFilesJSON) > MaxJSONBytes {
		return Result{}, fmt.Errorf("test and base artifact JSON sizes must be between 1 and %d bytes", MaxJSONBytes)
	}
	if len(request.Patch) == 0 || len(request.Patch) > MaxPatchBytes {
		return Result{}, fmt.Errorf("patch size must be between 1 and %d bytes", MaxPatchBytes)
	}
	files, err := decodeStringMap(request.MaterializedTestsJSON, "materialized tests")
	if err != nil {
		return Result{}, err
	}
	baseFiles, err := decodeStringMap(request.BaseFilesJSON, "base files")
	if err != nil {
		return Result{}, err
	}
	if !utf8.Valid(request.Patch) {
		return Result{}, fmt.Errorf("patch is not valid UTF-8")
	}
	patchDigest := digest(request.Patch)
	if patchDigest != request.ExpectedPatchDigest {
		return Result{}, fmt.Errorf("patch digest mismatch")
	}
	materializedDigest := digest(pythonCanonicalStringMap(files))
	if materializedDigest != request.ExpectedTestsDigest {
		return Result{}, fmt.Errorf("materialized tests digest mismatch")
	}
	baseDigest := digest(pythonCanonicalStringMap(baseFiles))
	if baseDigest != request.ExpectedBaseFilesDigest {
		return Result{}, fmt.Errorf("base files digest mismatch")
	}
	if err := validateFiles(request.ChangedFiles, request.AllowedPaths, files, baseFiles, request.Patch); err != nil {
		return Result{}, err
	}
	if request.Subject.Kind == "" || request.Subject.ID == "" || !validDigest(request.Subject.Digest) || request.WorkspaceSubject != request.Subject {
		return Result{}, fmt.Errorf("workspace must match the approved immutable subject identity")
	}
	root, err := openWorkspaceRoot(request.WorkspaceRoot)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()

	for _, filePath := range request.ChangedFiles {
		parent, name, err := openParent(root, filePath)
		if err != nil {
			return Result{}, err
		}
		content := []byte(files[filePath])
		existing, exists, err := readRegularFile(parent, name, MaxFileBytes)
		if err != nil {
			_ = parent.Close()
			return Result{}, fmt.Errorf("inspect workspace target %q: %w", filePath, err)
		}
		if !exists || !bytes.Equal(existing, content) {
			expectedBase, hasBase := baseFiles[filePath]
			if !exists && hasBase {
				_ = parent.Close()
				return Result{}, fmt.Errorf("base file missing from workspace: %s", filePath)
			}
			if exists && (!hasBase || !bytes.Equal(existing, []byte(expectedBase))) {
				_ = parent.Close()
				return Result{}, fmt.Errorf("workspace base content mismatch: %s", filePath)
			}
			if err := replaceFile(parent, name, content); err != nil {
				_ = parent.Close()
				return Result{}, fmt.Errorf("materialize %q: %w", filePath, err)
			}
		}
		actual, exists, err := readRegularFile(parent, name, MaxFileBytes)
		_ = parent.Close()
		if err != nil || !exists || !bytes.Equal(actual, content) {
			return Result{}, fmt.Errorf("materialized content verification failed: %s", filePath)
		}
	}
	result := Result{
		Subject: request.Subject, ChangedFiles: append([]string(nil), request.ChangedFiles...),
		AllowedPaths: append([]string(nil), request.AllowedPaths...), PatchDigest: patchDigest,
		MaterializedDigest: materializedDigest, BaseFilesDigest: baseDigest,
	}
	if err := VerifyMaterializedTests(request.WorkspaceRoot, result); err != nil {
		return Result{}, err
	}
	return result, nil
}

// MaterializeFromStore resolves only artifacts closed over by the approved
// generation manifest and immutable task specification.
func MaterializeFromStore(ctx context.Context, workspaceRoot string, workspaceSubject execution.Identity, taskSpec execution.TaskSpec, store artifact.Store) (Result, error) {
	if store == nil {
		return Result{}, fmt.Errorf("artifact store is required")
	}
	if err := taskSpec.Validate(); err != nil {
		return Result{}, fmt.Errorf("invalid approved task specification: %w", err)
	}
	manifestBytes, err := getArtifact(ctx, store, taskSpec.Generation.Manifest, MaxJSONBytes)
	if err != nil {
		return Result{}, fmt.Errorf("resolve generation manifest: %w", err)
	}
	var manifest generationManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Result{}, fmt.Errorf("decode generation manifest: %w", err)
	}
	refs := make(map[string]execution.Artifact, len(manifest.Artifacts))
	for _, ref := range manifest.Artifacts {
		if _, duplicate := refs[ref.ArtifactID]; duplicate {
			return Result{}, fmt.Errorf("generation manifest contains duplicate artifact IDs")
		}
		refs[ref.ArtifactID] = ref
	}
	baseRef, exists := refs[manifest.Handoff.BaseArtifactID]
	if !exists {
		return Result{}, fmt.Errorf("generation manifest does not reference base preimage artifact")
	}
	patchBytes, err := getArtifact(ctx, store, taskSpec.Generation.Patch, MaxPatchBytes)
	if err != nil {
		return Result{}, fmt.Errorf("resolve approved patch artifact: %w", err)
	}
	testsBytes, err := getArtifact(ctx, store, taskSpec.Generation.Tests, MaxJSONBytes)
	if err != nil {
		return Result{}, fmt.Errorf("resolve approved tests artifact: %w", err)
	}
	baseBytes, err := getArtifact(ctx, store, baseRef, MaxJSONBytes)
	if err != nil {
		return Result{}, fmt.Errorf("resolve approved base preimage artifact: %w", err)
	}
	request := Request{
		WorkspaceRoot: workspaceRoot, Subject: taskSpec.Subject.Identity, WorkspaceSubject: workspaceSubject,
		TaskSpec: taskSpec, GenerationManifestJSON: manifestBytes, Patch: patchBytes,
		MaterializedTestsJSON: testsBytes, BaseFilesJSON: baseBytes,
		AllowedPaths: manifest.Handoff.AllowedPaths, ChangedFiles: manifest.Handoff.ChangedFiles,
		ExpectedPatchDigest:     taskSpec.Generation.Patch.Digest,
		ExpectedTestsDigest:     taskSpec.Generation.Tests.Digest,
		ExpectedBaseFilesDigest: baseRef.Digest,
	}
	return MaterializeApprovedTests(request)
}

func getArtifact(ctx context.Context, store artifact.Store, reference execution.Artifact, limit int64) ([]byte, error) {
	if err := reference.Validate(); err != nil {
		return nil, err
	}
	content, err := store.Get(ctx, reference.Digest, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit || reference.SizeBytes > 0 && reference.SizeBytes != int64(len(content)) || digest(content) != reference.Digest {
		return nil, artifact.ErrCorrupt
	}
	return content, nil
}

type approvedMaterialization struct {
	AllowedPaths           []string
	ChangedFiles           []string
	PatchDigest            string
	MaterializedTestDigest string
	BaseFilesDigest        string
}

func verifyApprovedHandoff(request Request) (approvedMaterialization, error) {
	if err := request.TaskSpec.Validate(); err != nil {
		return approvedMaterialization{}, fmt.Errorf("invalid approved execution task specification: %w", err)
	}
	if len(request.GenerationManifestJSON) == 0 || len(request.GenerationManifestJSON) > MaxJSONBytes {
		return approvedMaterialization{}, fmt.Errorf("approved generation manifest size is invalid")
	}
	manifestDigest := execution.DigestTaskSpec(request.GenerationManifestJSON)
	if manifestDigest != request.TaskSpec.Generation.Manifest.Digest || manifestDigest != request.TaskSpec.CodeApproval.ReviewedGenerationDigest {
		return approvedMaterialization{}, fmt.Errorf("generation manifest differs from reviewed approval digest")
	}
	var manifest generationManifest
	if err := json.Unmarshal(request.GenerationManifestJSON, &manifest); err != nil {
		return approvedMaterialization{}, fmt.Errorf("decode approved generation manifest: %w", err)
	}
	if manifest.Contract != "atg.generation" || manifest.Version != "1.0" || manifest.GenerationID != request.TaskSpec.GenerationID || manifest.RunID != request.TaskSpec.RunID || manifest.Status != "generated" && manifest.Status != "verified" {
		return approvedMaterialization{}, fmt.Errorf("generation manifest identity or status differs from approved task")
	}
	selectedSubject := manifest.Base
	if request.TaskSpec.SubjectRole == "candidate" {
		selectedSubject = manifest.Candidate
	}
	if !identityMatches(selectedSubject, request.TaskSpec.Subject.Identity) || request.Subject != request.TaskSpec.Subject.Identity || request.WorkspaceSubject != request.TaskSpec.Subject.Identity {
		return approvedMaterialization{}, fmt.Errorf("workspace and task subject do not match the approved generation subject")
	}
	handoff := manifest.Handoff
	if handoff.Status != "ready" || handoff.GenerationID != manifest.GenerationID || handoff.RunID != manifest.RunID ||
		handoff.PlanID != manifest.PlanID || handoff.PlanRevision != manifest.PlanRevision || handoff.PlanArtifactDigest != manifest.PlanArtifactDigest ||
		handoff.PlanCheckpointID != manifest.PlanCheckpointID || handoff.PlanDecisionID != manifest.PlanDecisionID ||
		handoff.PatchDigest != manifest.PatchDigest || handoff.MaterializedTestDigest != manifest.MaterializedTestDigest ||
		!slices.Equal(handoff.ChangedFiles, manifest.ChangedFiles) || len(manifest.Attempts) == 0 {
		return approvedMaterialization{}, fmt.Errorf("generation materialization handoff is incomplete or inconsistent")
	}
	if manifest.PatchDigest != request.TaskSpec.Generation.Patch.Digest || manifest.MaterializedTestDigest != request.TaskSpec.Generation.Tests.Digest ||
		handoff.PatchArtifactID != request.TaskSpec.Generation.Patch.ArtifactID || handoff.MaterializedArtifactID != request.TaskSpec.Generation.Tests.ArtifactID {
		return approvedMaterialization{}, fmt.Errorf("generation handoff artifact references differ from the immutable task specification")
	}
	lastAttempt := manifest.Attempts[len(manifest.Attempts)-1]
	if lastAttempt.PatchArtifactID != handoff.PatchArtifactID || lastAttempt.MaterializedArtifactID != handoff.MaterializedArtifactID ||
		lastAttempt.BaseArtifactID != handoff.BaseArtifactID || lastAttempt.OutputDigest != handoff.PatchDigest || !slices.Equal(lastAttempt.ChangedFiles, handoff.ChangedFiles) || lastAttempt.Status != "verified" {
		return approvedMaterialization{}, fmt.Errorf("generation handoff differs from the final verified attempt")
	}
	if len(manifest.SubjectMaterializations) != 2 || len(handoff.Subjects) != 2 {
		return approvedMaterialization{}, fmt.Errorf("generation handoff must bind both comparison subjects")
	}
	for _, subject := range []execution.Identity{manifest.Base, manifest.Candidate} {
		found := false
		for _, materialization := range manifest.SubjectMaterializations {
			if identityMatches(materialization.Subject, subject) && materialization.TestDigest == manifest.MaterializedTestDigest {
				found = true
				break
			}
		}
		if !found {
			return approvedMaterialization{}, fmt.Errorf("generation subject materialization does not bind the approved tests")
		}
		found = false
		for _, materialization := range handoff.Subjects {
			if identityMatches(materialization.Subject, subject) && materialization.TestDigest == manifest.MaterializedTestDigest {
				found = true
				break
			}
		}
		if !found {
			return approvedMaterialization{}, fmt.Errorf("handoff subject materialization does not bind the approved tests")
		}
	}
	artifacts := make(map[string]execution.Artifact, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if err := artifact.Validate(); err != nil {
			return approvedMaterialization{}, fmt.Errorf("generation artifact metadata: %w", err)
		}
		if _, duplicate := artifacts[artifact.ArtifactID]; duplicate {
			return approvedMaterialization{}, fmt.Errorf("generation manifest contains duplicate artifact IDs")
		}
		artifacts[artifact.ArtifactID] = artifact
	}
	patchArtifact, patchFound := artifacts[handoff.PatchArtifactID]
	testsArtifact, testsFound := artifacts[handoff.MaterializedArtifactID]
	baseArtifact, baseFound := artifacts[handoff.BaseArtifactID]
	if !patchFound || !testsFound || !baseFound || patchArtifact.MediaType != request.TaskSpec.Generation.Patch.MediaType || patchArtifact.Digest != manifest.PatchDigest ||
		testsArtifact.MediaType != request.TaskSpec.Generation.Tests.MediaType || testsArtifact.Digest != manifest.MaterializedTestDigest ||
		baseArtifact.MediaType != "application/vnd.atg.base-files+json" {
		return approvedMaterialization{}, fmt.Errorf("generation artifact metadata does not close over the approved handoff")
	}
	if !slices.Equal(request.AllowedPaths, handoff.AllowedPaths) || !slices.Equal(request.ChangedFiles, handoff.ChangedFiles) ||
		request.ExpectedPatchDigest != handoff.PatchDigest || request.ExpectedTestsDigest != handoff.MaterializedTestDigest ||
		request.ExpectedBaseFilesDigest != baseArtifact.Digest {
		return approvedMaterialization{}, fmt.Errorf("materialization inputs differ from the approved handoff")
	}
	return approvedMaterialization{
		AllowedPaths: append([]string(nil), handoff.AllowedPaths...), ChangedFiles: append([]string(nil), handoff.ChangedFiles...),
		PatchDigest: handoff.PatchDigest, MaterializedTestDigest: handoff.MaterializedTestDigest, BaseFilesDigest: baseArtifact.Digest,
	}, nil
}

func identityMatches(manifestIdentity, taskIdentity execution.Identity) bool {
	return manifestIdentity.Kind == taskIdentity.Kind && manifestIdentity.ID == taskIdentity.ID &&
		(manifestIdentity.Version == "" || manifestIdentity.Version == taskIdentity.Version) &&
		(manifestIdentity.Digest == "" || manifestIdentity.Digest == taskIdentity.Digest)
}

func validateFiles(changedFiles, allowedPaths []string, files, baseFiles map[string]string, patch []byte) error {
	if len(files) != len(changedFiles) || len(baseFiles) > MaxFileCount {
		return fmt.Errorf("changed file inventory must match 1..%d materialized tests", MaxFileCount)
	}
	seen, err := validateChangedPathScope(changedFiles, allowedPaths)
	if err != nil {
		return err
	}
	var total int64
	for _, filePath := range changedFiles {
		content, exists := files[filePath]
		if !exists {
			return fmt.Errorf("changed file is absent from materialized tests: %s", filePath)
		}
		if len(content) > MaxFileBytes {
			return fmt.Errorf("materialized file exceeds size limit: %s", filePath)
		}
		total += int64(len(content))
	}
	if total > MaxTotalBytes {
		return fmt.Errorf("materialized tests exceed total size limit")
	}
	for basePath, content := range baseFiles {
		if len(content) > MaxFileBytes {
			return fmt.Errorf("base file exceeds size limit: %s", basePath)
		}
	}
	patchPaths, err := derivePatchPaths(patch)
	if err != nil {
		return err
	}
	if len(patchPaths) != len(seen) {
		return fmt.Errorf("patch paths differ from changed file inventory")
	}
	for _, filePath := range patchPaths {
		if _, exists := seen[filePath]; !exists {
			return fmt.Errorf("patch path is not an approved materialized file: %s", filePath)
		}
	}
	return verifyPatchMaterialization(patch, patchPaths, baseFiles, files)
}

func validateChangedPathScope(changedFiles, allowedPaths []string) (map[string]struct{}, error) {
	if len(changedFiles) == 0 || len(changedFiles) > MaxFileCount {
		return nil, fmt.Errorf("changed file inventory must contain 1..%d paths", MaxFileCount)
	}
	if len(allowedPaths) == 0 || len(allowedPaths) > MaxPathRoots {
		return nil, fmt.Errorf("approved plan must contain 1..%d allowed paths", MaxPathRoots)
	}
	allowed := make(map[string]struct{}, len(allowedPaths))
	for _, allowedPath := range allowedPaths {
		trimmedRoot := strings.TrimSuffix(allowedPath, "/")
		if allowedPath == "" || trimmedRoot == "" || strings.HasPrefix(allowedPath, "/") || strings.ContainsAny(allowedPath, "\\\x00") || path.Clean(trimmedRoot) != trimmedRoot || trimmedRoot == ".." || strings.HasPrefix(trimmedRoot, "../") {
			return nil, fmt.Errorf("invalid approved path root %q", allowedPath)
		}
		if strings.HasSuffix(allowedPath, "/") {
			trimmedRoot += "/"
		}
		allowed[trimmedRoot] = struct{}{}
	}
	seen := make(map[string]struct{}, len(changedFiles))
	for _, filePath := range changedFiles {
		if filePath == "" || strings.HasPrefix(filePath, "/") || strings.ContainsAny(filePath, "\\\x00") || path.Clean(filePath) != filePath || strings.HasPrefix(filePath, "../") || !supportedPath.MatchString(filePath) {
			return nil, fmt.Errorf("unsafe or unsupported changed file path %q", filePath)
		}
		if _, exists := seen[filePath]; exists {
			return nil, fmt.Errorf("duplicate changed file path %q", filePath)
		}
		seen[filePath] = struct{}{}
		permitted := false
		for root := range allowed {
			if strings.HasSuffix(root, "/") && strings.HasPrefix(filePath, root) || !strings.HasSuffix(root, "/") && (filePath == root || strings.HasPrefix(filePath, root+"/")) {
				permitted = true
				break
			}
		}
		if !permitted {
			return nil, fmt.Errorf("changed file is outside approved paths: %s", filePath)
		}
	}
	return seen, nil
}

// VerifyMaterializedTests rechecks immutable test bytes immediately before a
// profile command is launched, detecting workspace mutation after staging.
func VerifyMaterializedTests(workspaceRoot string, expected Result) error {
	if !validDigest(expected.MaterializedDigest) || expected.Subject.Kind == "" || expected.Subject.ID == "" || !validDigest(expected.Subject.Digest) {
		return fmt.Errorf("expected materialization identity or digest is invalid")
	}
	if _, err := validateChangedPathScope(expected.ChangedFiles, expected.AllowedPaths); err != nil {
		return err
	}
	root, err := openWorkspaceRoot(workspaceRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	files := make(map[string]string, len(expected.ChangedFiles))
	var total int64
	for _, filePath := range expected.ChangedFiles {
		parent, name, err := openParent(root, filePath)
		if err != nil {
			return err
		}
		content, exists, readErr := readRegularFile(parent, name, MaxFileBytes)
		_ = parent.Close()
		if readErr != nil || !exists {
			return fmt.Errorf("verified materialized file is missing or unsafe: %s", filePath)
		}
		total += int64(len(content))
		if total > MaxTotalBytes {
			return fmt.Errorf("materialized tests exceed total size limit")
		}
		files[filePath] = string(content)
	}
	if digest(pythonCanonicalStringMap(files)) != expected.MaterializedDigest {
		return fmt.Errorf("materialized test digest changed after staging")
	}
	return nil
}

func derivePatchPaths(patch []byte) ([]string, error) {
	var paths []string
	for len(patch) > 0 {
		line := patch
		if newline := bytes.IndexByte(patch, '\n'); newline >= 0 {
			line, patch = patch[:newline], patch[newline+1:]
		} else {
			patch = nil
		}
		if !bytes.HasPrefix(line, []byte("diff --git ")) {
			continue
		}
		if len(line) > 4096 {
			return nil, fmt.Errorf("patch file header exceeds path size limit")
		}
		fields := strings.Fields(string(line))
		if len(fields) != 4 || fields[0] != "diff" || fields[1] != "--git" || !strings.HasPrefix(fields[2], "a/") || !strings.HasPrefix(fields[3], "b/") {
			return nil, fmt.Errorf("malformed patch file header")
		}
		before, after := strings.TrimPrefix(fields[2], "a/"), strings.TrimPrefix(fields[3], "b/")
		if before != after {
			return nil, fmt.Errorf("renamed patch paths are not supported")
		}
		paths = append(paths, after)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("patch contains no file sections")
	}
	if !sort.StringsAreSorted(paths) {
		return nil, fmt.Errorf("patch file sections are not in canonical order")
	}
	for index := 1; index < len(paths); index++ {
		if paths[index-1] == paths[index] {
			return nil, fmt.Errorf("patch contains duplicate file sections")
		}
	}
	return paths, nil
}

type patchLine struct {
	operation byte
	content   string
	noNewline bool
}

func verifyPatchMaterialization(patch []byte, paths []string, baseFiles, materializedFiles map[string]string) error {
	lines := splitPatchLines(patch)
	lineIndex := 0
	for _, filePath := range paths {
		if lineIndex >= len(lines) || !strings.HasPrefix(lines[lineIndex], "diff --git ") {
			return fmt.Errorf("patch section is missing for %s", filePath)
		}
		fields := strings.Fields(strings.TrimSuffix(lines[lineIndex], "\n"))
		if len(fields) != 4 || fields[2] != "a/"+filePath || fields[3] != "b/"+filePath {
			return fmt.Errorf("patch section path differs from approved file %s", filePath)
		}
		lineIndex++
		created := false
		if lineIndex < len(lines) && lines[lineIndex] == "new file mode 100644\n" {
			created = true
			lineIndex++
		}
		if lineIndex+1 >= len(lines) || !strings.HasPrefix(lines[lineIndex], "--- ") || !strings.HasPrefix(lines[lineIndex+1], "+++ ") {
			return fmt.Errorf("patch file headers are incomplete for %s", filePath)
		}
		oldName := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(lines[lineIndex], "\n"), "--- "), "\r")
		newName := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(lines[lineIndex+1], "\n"), "+++ "), "\r")
		before, existed := baseFiles[filePath]
		if created == existed || existed && (oldName != "a/"+filePath || newName != "b/"+filePath) || !existed && (oldName != "/dev/null" || newName != "b/"+filePath) {
			return fmt.Errorf("patch preimage headers do not match base artifact for %s", filePath)
		}
		lineIndex += 2
		baseLines := splitLFContent(before)
		var output []string
		basePosition := 0
		for lineIndex < len(lines) && !strings.HasPrefix(lines[lineIndex], "diff --git ") {
			if !strings.HasPrefix(lines[lineIndex], "@@ ") {
				return fmt.Errorf("unexpected patch metadata for %s", filePath)
			}
			oldStart, oldCount, newStart, newCount, err := parseHunkRange(lines[lineIndex])
			if err != nil {
				return err
			}
			lineIndex++
			body, nextIndex, err := parseHunkBody(lines, lineIndex, oldCount, newCount)
			if err != nil {
				return fmt.Errorf("invalid patch hunk for %s: %w", filePath, err)
			}
			lineIndex = nextIndex
			oldPosition := oldStart
			if oldCount > 0 {
				oldPosition--
			}
			newPosition := newStart
			if newCount > 0 {
				newPosition--
			}
			if oldPosition < basePosition || oldPosition > len(baseLines) || newPosition != len(output)+oldPosition-basePosition {
				return fmt.Errorf("patch hunk ranges do not match file preimage for %s", filePath)
			}
			output = append(output, baseLines[basePosition:oldPosition]...)
			basePosition = oldPosition
			var consumedOld, consumedNew int
			for _, change := range body {
				switch change.operation {
				case ' ':
					if basePosition >= len(baseLines) || baseLines[basePosition] != change.content {
						return fmt.Errorf("patch context differs from base file %s", filePath)
					}
					output = append(output, change.content)
					basePosition++
					consumedOld++
					consumedNew++
				case '-':
					if basePosition >= len(baseLines) || baseLines[basePosition] != change.content {
						return fmt.Errorf("patch deletion differs from base file %s", filePath)
					}
					basePosition++
					consumedOld++
				case '+':
					output = append(output, change.content)
					consumedNew++
				}
			}
			if consumedOld != oldCount || consumedNew != newCount {
				return fmt.Errorf("patch hunk counts do not match for %s", filePath)
			}
		}
		output = append(output, baseLines[basePosition:]...)
		if strings.Join(output, "") != materializedFiles[filePath] {
			return fmt.Errorf("patch does not reproduce approved materialized bytes for %s", filePath)
		}
	}
	if lineIndex != len(lines) {
		return fmt.Errorf("patch contains unbound file sections")
	}
	return nil
}

func splitPatchLines(patch []byte) []string {
	if len(patch) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(patch), "\n")
	if lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}
	return lines
}

func splitLFContent(content string) []string {
	if content == "" {
		return nil
	}
	parts := strings.Split(content, "\n")
	lines := make([]string, 0, len(parts))
	for _, part := range parts[:len(parts)-1] {
		lines = append(lines, part+"\n")
	}
	if parts[len(parts)-1] != "" {
		lines = append(lines, parts[len(parts)-1])
	}
	return lines
}

func parseHunkRange(line string) (oldStart, oldCount, newStart, newCount int, err error) {
	match := unifiedHunkHeader.FindStringSubmatch(strings.TrimSuffix(line, "\n"))
	if match == nil {
		return 0, 0, 0, 0, fmt.Errorf("malformed unified patch hunk header")
	}
	parse := func(value string, defaultValue int) (int, error) {
		if value == "" {
			return defaultValue, nil
		}
		var number int
		_, scanErr := fmt.Sscanf(value, "%d", &number)
		return number, scanErr
	}
	if oldStart, err = parse(match[1], 0); err != nil {
		return 0, 0, 0, 0, err
	}
	if oldCount, err = parse(match[2], 1); err != nil {
		return 0, 0, 0, 0, err
	}
	if newStart, err = parse(match[3], 0); err != nil {
		return 0, 0, 0, 0, err
	}
	if newCount, err = parse(match[4], 1); err != nil {
		return 0, 0, 0, 0, err
	}
	return oldStart, oldCount, newStart, newCount, nil
}

func parseHunkBody(lines []string, start, oldCount, newCount int) ([]patchLine, int, error) {
	var body []patchLine
	var consumedOld, consumedNew int
	index := start
	for index < len(lines) {
		line := lines[index]
		if strings.HasPrefix(line, "\\ No newline at end of file") {
			if len(body) == 0 || body[len(body)-1].noNewline {
				return nil, 0, fmt.Errorf("orphan no-newline marker")
			}
			body[len(body)-1].noNewline = true
			index++
			continue
		}
		if consumedOld == oldCount && consumedNew == newCount {
			break
		}
		if len(line) < 2 || line[0] != ' ' && line[0] != '+' && line[0] != '-' || !strings.HasSuffix(line, "\n") {
			return nil, 0, fmt.Errorf("malformed unified patch line")
		}
		change := patchLine{operation: line[0], content: line[1:]}
		body = append(body, change)
		switch change.operation {
		case ' ':
			consumedOld++
			consumedNew++
		case '-':
			consumedOld++
		case '+':
			consumedNew++
		}
		if consumedOld > oldCount || consumedNew > newCount {
			return nil, 0, fmt.Errorf("unified patch hunk exceeds declared range")
		}
		index++
	}
	if consumedOld != oldCount || consumedNew != newCount {
		return nil, 0, fmt.Errorf("unified patch hunk is truncated")
	}
	for index := range body {
		if body[index].noNewline {
			body[index].content = strings.TrimSuffix(body[index].content, "\n")
		}
	}
	return body, index, nil
}

func decodeStringMap(data []byte, label string) (map[string]string, error) {
	if len(data) == 0 || !utf8.Valid(data) {
		return nil, fmt.Errorf("%s artifact is empty or not valid UTF-8", label)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("%s artifact must be a JSON object", label)
	}
	values := make(map[string]string)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode %s key: %w", label, err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("%s keys must be strings", label)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("%s artifact contains duplicate key %q", label, key)
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("%s values must be strings: %w", label, err)
		}
		values[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("finish %s object: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s artifact must contain one JSON object", label)
	}
	return values, nil
}

func pythonCanonicalStringMap(values map[string]string) []byte {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var buffer strings.Builder
	buffer.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			buffer.WriteByte(',')
		}
		writePythonJSONString(&buffer, key)
		buffer.WriteByte(':')
		writePythonJSONString(&buffer, values[key])
	}
	buffer.WriteByte('}')
	return []byte(buffer.String())
}

func writePythonJSONString(buffer *strings.Builder, value string) {
	buffer.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		default:
			if char < 0x20 {
				fmt.Fprintf(buffer, `\u%04x`, char)
			} else if char > 0x7f {
				for _, unit := range utf16.Encode([]rune{char}) {
					fmt.Fprintf(buffer, `\u%04x`, unit)
				}
			} else {
				buffer.WriteRune(char)
			}
		}
	}
	buffer.WriteByte('"')
}

func openWorkspaceRoot(rootPath string) (*os.Root, error) {
	if strings.TrimSpace(rootPath) == "" {
		return nil, fmt.Errorf("workspace root is required")
	}
	absolute, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	before, err := os.Lstat(absolute)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("workspace root must be a real directory")
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	opened, err := root.Stat(".")
	after, afterErr := os.Lstat(absolute)
	if err != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
		_ = root.Close()
		return nil, fmt.Errorf("workspace root changed while opening")
	}
	return root, nil
}

func openParent(root *os.Root, filePath string) (*os.Root, string, error) {
	parts := strings.Split(filePath, "/")
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, "", fmt.Errorf("open workspace root for path traversal: %w", err)
	}
	owned := true
	for _, component := range parts[:len(parts)-1] {
		before, err := current.Lstat(component)
		if errors.Is(err, os.ErrNotExist) {
			if err := current.Mkdir(component, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				if owned {
					_ = current.Close()
				}
				return nil, "", fmt.Errorf("create workspace directory: %w", err)
			}
			before, err = current.Lstat(component)
		}
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			if owned {
				_ = current.Close()
			}
			return nil, "", fmt.Errorf("workspace path contains a symlink or non-directory")
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			if owned {
				_ = current.Close()
			}
			return nil, "", fmt.Errorf("open workspace directory: %w", err)
		}
		opened, openedErr := next.Stat(".")
		after, afterErr := current.Lstat(component)
		if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
			_ = next.Close()
			if owned {
				_ = current.Close()
			}
			return nil, "", fmt.Errorf("workspace directory changed while opening")
		}
		if owned {
			_ = current.Close()
		}
		current, owned = next, true
	}
	return current, parts[len(parts)-1], nil
}

func readRegularFile(root *os.Root, name string, limit int64) ([]byte, bool, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, true, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("workspace target is not a regular file")
	}
	if info.Size() > limit {
		return nil, true, fmt.Errorf("workspace base file exceeds size limit")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, true, err
	}
	if int64(len(content)) > limit {
		return nil, true, fmt.Errorf("workspace base file exceeds size limit")
	}
	return content, true, nil
}

func replaceFile(parent *os.Root, name string, content []byte) error {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("generate temporary file identity: %w", err)
	}
	temporary := ".atg-" + hex.EncodeToString(raw[:]) + ".tmp"
	file, err := parent.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = parent.Remove(temporary)
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return fmt.Errorf("temporary materialization target is not a regular file")
	}
	for len(content) > 0 {
		written, err := file.Write(content)
		if err != nil {
			_ = file.Close()
			return err
		}
		if written == 0 {
			_ = file.Close()
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := parent.Rename(temporary, name); err != nil {
		return err
	}
	cleanup = false
	directory, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func digest(content []byte) string {
	value := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(value[:])
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(value[len("sha256:"):])
	return err == nil && value == strings.ToLower(value)
}
