package agentworker

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// The policy intentionally uses only Docker's universal exclusion and literal
// file exceptions. This subset needs no engine or downloaded pattern matcher.
// Docker matches parents too: reopening a directory reopens all descendants.
func literalDockerContextIncludes(patterns []string, name string) bool {
	included := false
	for _, pattern := range patterns {
		if pattern == "**" {
			included = false
			continue
		}
		literal := strings.TrimSuffix(strings.TrimPrefix(pattern, "!"), "/")
		if name == literal || strings.HasPrefix(name, literal+"/") {
			included = strings.HasPrefix(pattern, "!")
		}
	}
	return included
}

func TestGenerationBuildContextIncludesOnlyExplicitRegularInputs(t *testing.T) {
	ignore := repositoryAsset(t, ".dockerignore")
	raw, err := os.ReadFile(ignore)
	if err != nil {
		t.Fatal(err)
	}
	var patterns []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, line)
		}
	}
	if len(patterns) == 0 || patterns[0] != "**" {
		t.Fatal("generation context must exclude everything by default")
	}
	wanted := map[string]bool{
		"services/generation-worker/pyproject.toml": true,
		"workers/generation/Containerfile":          true, "workers/generation/entrypoint.sh": true,
	}
	for _, name := range []string{"__init__.py", "__main__.py", "harnesses.py", "protocol.py", "worker.py"} {
		wanted["services/generation-worker/src/atg_generation_worker/"+name] = true
	}
	if len(patterns) != len(wanted)+1 {
		t.Fatalf("unexpected context rules: %v", patterns)
	}
	for _, pattern := range patterns[1:] {
		if !strings.HasPrefix(pattern, "!") || strings.ContainsAny(pattern, "*?[]\\") || strings.HasSuffix(pattern, "/") {
			t.Fatalf("context rules may only name literal files, never parent directories: %q", pattern)
		}
		name := strings.TrimPrefix(pattern, "!")
		if path.Clean(name) != name || !wanted[name] {
			t.Fatalf("unexpected context inclusion: %q", name)
		}
		info, err := os.Lstat(filepath.Join(filepath.Dir(ignore), filepath.FromSlash(name)))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("context input must be a regular file, not a directory/symlink: %q %v", name, err)
		}
		if !literalDockerContextIncludes(patterns, name) {
			t.Fatalf("required build input excluded: %s", name)
		}
		delete(wanted, name)
	}
	if len(wanted) != 0 {
		t.Fatalf("missing context inputs: %v", wanted)
	}
	for _, name := range []string{".env", ".git/config", ".kilo/evidence/task.json", ".venv/private-key", "services/control/.env", "services/generation-worker/.env", "services/generation-worker/.venv/private-key", "services/generation-worker/evidence/task.json", "services/generation-worker/src/atg_generation_worker/credential.py", "workers/generation/private-key"} {
		if literalDockerContextIncludes(patterns, name) {
			t.Fatalf("sensitive sentinel would enter generation context: %s", name)
		}
	}
	if !literalDockerContextIncludes([]string{"**", "!services/"}, "services/generation-worker/.env") {
		t.Fatal("context regression check must account for Docker parent-directory matching")
	}
}
