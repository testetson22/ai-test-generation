package agentworker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/localworker"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

type SourceFile struct {
	Path    string `json:"path"`
	Digest  string `json:"digest"`
	Content string `json:"content"`
}

type Source struct {
	RepositoryURL string       `json:"repository_url"`
	Commit        string       `json:"commit"`
	TreeID        string       `json:"tree_id"`
	ArtifactRef   string       `json:"artifact_ref"`
	Files         []SourceFile `json:"files"`
}

func (w *Worker) source(ctx context.Context, r testgenflow.WorkRequest) (Source, error) {
	refs, err := w.sourceEvidence(ctx, r.RunID)
	if err != nil {
		return Source{}, fmt.Errorf("source evidence unavailable")
	}
	if len(refs) == 0 || len(refs) > 128 {
		return Source{}, fmt.Errorf("source evidence inventory missing or oversized")
	}
	var selected *localworker.Evidence
	selectedRef := ""
	for _, ref := range refs {
		raw, err := w.get(ctx, ref, 256*1024)
		if err != nil {
			return Source{}, err
		}
		var e localworker.Evidence
		if err = strictJSON(raw, &e); err != nil {
			return Source{}, fmt.Errorf("invalid source evidence manifest")
		}
		if e.Phase != "source" {
			continue
		}
		if e.SchemaVersion != "1" || !e.Succeeded || e.Partial || e.ExitCode != 0 || e.ProfileDigest != r.Request.ProfileDigest || e.SourceCommit != r.ResolvedCommit || e.SourceCommit != r.Request.Revision || e.WorkerID == "" || !digestPattern.MatchString(e.InputDigest) || !digestPattern.MatchString(e.ImageID) {
			return Source{}, fmt.Errorf("source evidence does not bind completed pinned source/profile")
		}
		if selected != nil && selectedRef != ref {
			return Source{}, fmt.Errorf("ambiguous source evidence")
		}
		selected, selectedRef = &e, ref
	}
	if selected == nil {
		return Source{}, fmt.Errorf("completed source evidence is required")
	}
	// The callback is the run-ownership boundary; the retained profile and source
	// identity independently bind repository, commit, tree and immutable image.
	p, sourceImage, err := w.sourceProfile(ctx, r.Request.ProfileDigest)
	if err != nil || sourceImage != selected.ImageID || p.RepositoryURL != r.Request.RepositoryURL || p.Commit != r.ResolvedCommit || p.ID != r.Request.ProfileID || p.Version != r.Request.ProfileVersion || p.Subdirectory != r.Request.Subdirectory {
		return Source{}, fmt.Errorf("source profile differs from request")
	}
	identity, err := w.get(ctx, selected.Artifacts["source-identity"], 1024)
	if err != nil {
		return Source{}, err
	}
	parts := strings.Split(string(identity), "\n")
	if len(parts) != 3 || parts[0] != r.ResolvedCommit || !commitPattern.MatchString(parts[1]) || parts[2] != "" {
		return Source{}, fmt.Errorf("invalid source commit/tree identity")
	}
	archiveRef := selected.Artifacts["source.tar.gz"]
	archive, err := w.get(ctx, archiveRef, maxArchiveBytes)
	if err != nil {
		return Source{}, err
	}
	files, err := contextFiles(archive, w.policy.ContextPaths, w.policy.Limits.MaxContextBytes)
	if err != nil {
		return Source{}, err
	}
	return Source{RepositoryURL: p.RepositoryURL, Commit: p.Commit, TreeID: parts[1], ArtifactRef: archiveRef, Files: files}, nil
}

func (w *Worker) sourceProfile(ctx context.Context, ref string) (localworker.Profile, string, error) {
	bound, err := w.get(ctx, ref, 256*1024)
	if err != nil {
		return localworker.Profile{}, "", err
	}
	var binding struct {
		SchemaVersion string          `json:"schema_version"`
		ProfileDigest string          `json:"profile_digest"`
		Profile       json.RawMessage `json:"profile"`
		LocalImageID  string          `json:"local_image_id"`
	}
	if strictJSON(bound, &binding) != nil || binding.SchemaVersion != "1" || !digestPattern.MatchString(binding.LocalImageID) {
		return localworker.Profile{}, "", fmt.Errorf("invalid source profile binding")
	}
	profileRaw, err := w.get(ctx, binding.ProfileDigest, 256*1024)
	if err != nil {
		return localworker.Profile{}, "", err
	}
	var compact bytes.Buffer
	if json.Compact(&compact, profileRaw) != nil || !bytes.Equal(compact.Bytes(), binding.Profile) {
		return localworker.Profile{}, "", fmt.Errorf("source profile bytes differ from retained binding")
	}
	p, err := localworker.DecodeProfile(profileRaw)
	return p, binding.LocalImageID, err
}

// Read the archive in memory. Never extract repository-controlled paths onto the
// host, and never treat repository text as worker instructions or authorization.
func contextFiles(raw []byte, paths []string, maxBytes int) ([]SourceFile, error) {
	return inspectArchive(raw, paths, maxBytes, false)
}

func (w *Worker) newTestFiles(ctx context.Context, t Task, files []localworker.TestFile) error {
	raw, err := w.get(ctx, t.Source.ArtifactRef, maxArchiveBytes)
	if err != nil {
		return err
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	_, err = inspectArchive(raw, paths, 0, true)
	return err
}

func inspectArchive(raw []byte, paths []string, maxBytes int, rejectExisting bool) ([]SourceFile, error) {
	if len(raw) == 0 || len(raw) > maxArchiveBytes {
		return nil, artifact.ErrTooLarge
	}
	compressed := bytes.NewReader(raw)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("invalid source gzip")
	}
	defer gz.Close()
	gz.Multistream(false)
	bounded := &io.LimitedReader{R: gz, N: 64*1024*1024 + 1}
	reader := tar.NewReader(bounded)
	wanted, seen := map[string]bool{}, map[string]bool{}
	for _, name := range paths {
		if !safePath(name) || wanted[name] {
			return nil, fmt.Errorf("invalid selected context path")
		}
		wanted[name] = true
	}
	var files []SourceFile
	total, count := int64(0), 0
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid source tar")
		}
		count++
		if count > 10000 || bounded.N <= 0 {
			return nil, artifact.ErrTooLarge
		}
		// Git archive may carry a global commit comment; no path/link overrides.
		if h.Typeflag == tar.TypeXGlobalHeader && count == 1 && len(h.PAXRecords) == 1 && h.PAXRecords["comment"] != "" {
			continue
		}
		name := strings.TrimSuffix(h.Name, "/")
		if !safePath(name) || seen[name] || h.Linkname != "" || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) || h.Size < 0 || h.Size > 32*1024*1024 {
			return nil, fmt.Errorf("unsafe, duplicate, linked or oversized source entry")
		}
		seen[name] = true
		if !wanted[name] {
			continue
		}
		if rejectExisting {
			return nil, fmt.Errorf("generated test would overwrite existing source")
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("selected context path is not a regular file")
		}
		total += h.Size
		if total > int64(maxBytes) {
			return nil, artifact.ErrTooLarge
		}
		content, err := io.ReadAll(io.LimitReader(reader, h.Size+1))
		if err != nil || int64(len(content)) != h.Size || !utf8.Valid(content) || bytes.ContainsRune(content, 0) {
			return nil, fmt.Errorf("invalid context text")
		}
		files = append(files, SourceFile{Path: name, Digest: artifact.Digest(content), Content: string(content)})
	}
	// Check gzip checksum and permit only zero padding after tar end markers.
	// A second hidden tar stream must not evade duplicate/path validation.
	padding := make([]byte, 32*1024)
	for {
		n, err := bounded.Read(padding)
		for _, b := range padding[:n] {
			if b != 0 {
				return nil, fmt.Errorf("unexpected data after source tar")
			}
		}
		if bounded.N <= 0 {
			return nil, artifact.ErrTooLarge
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("corrupt source gzip")
		}
	}
	if compressed.Len() != 0 {
		return nil, fmt.Errorf("concatenated source gzip")
	}
	if !rejectExisting && len(files) != len(wanted) {
		return nil, fmt.Errorf("selected context files missing from source")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
