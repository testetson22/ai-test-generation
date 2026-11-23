package localworker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
)

const (
	SourceURL    = "https://github.com/lzspangler/spring-boot-example-application"
	SourceCommit = "01a18a1b40e151ffcc1d7bef86bf335f87999750"
	DefaultImage = "localhost/atg-java17-maven:local"
	workerLabel  = "java17-maven-v1"
	BaseImage    = "docker.io/library/maven@sha256:7d92f4b97ceca94699c70dfe41e186115de1a9e34dd50413ab3f96ce32bc60ec"
)

type Sandbox struct {
	MemoryMiB    int `json:"memory_mib"`
	WorkspaceMiB int `json:"workspace_mib"`
	TemporaryMiB int `json:"temporary_mib"`
	PIDs         int `json:"pids"`
	CPUs         int `json:"cpus"`
	FileMiB      int `json:"file_mib"`
}

type Profile struct {
	SchemaVersion    string              `json:"schema_version"`
	ID               string              `json:"id"`
	Version          string              `json:"version"`
	RepositoryURL    string              `json:"repository_url"`
	Commit           string              `json:"commit"`
	Subdirectory     string              `json:"subdirectory"`
	NetworkPolicy    string              `json:"network_policy"`
	BaseImage        string              `json:"base_image"`
	WorkerRecipe     string              `json:"worker_recipe"`
	Sandbox          Sandbox             `json:"sandbox"`
	MavenGoals       map[string][]string `json:"maven_goals"`
	DeadlineSeconds  int                 `json:"deadline_seconds"`
	RetentionSeconds int                 `json:"retention_seconds"`
	BaselineTests    map[string][]string `json:"baseline_tests"`
	Digest           string              `json:"-"`
	raw              []byte
}

func LoadProfile(path string) (Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return Profile{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 128*1024+1))
	if err != nil {
		return Profile{}, err
	}
	return DecodeProfile(raw)
}

func DecodeProfile(raw []byte) (Profile, error) {
	var p Profile
	if len(raw) > 128*1024 {
		return p, fmt.Errorf("profile exceeds byte limit")
	}
	if err := strictJSON(raw, &p); err != nil {
		return p, err
	}
	p.Digest, p.raw = artifact.Digest(raw), append([]byte(nil), raw...)
	if err := p.validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

func (p Profile) validate() error {
	if p.SchemaVersion != "1" || p.ID != "ssc-java17-maven" || p.Version != "1.0.0" || p.RepositoryURL != SourceURL || p.Commit != SourceCommit || p.Subdirectory != "." || p.NetworkPolicy != "public-egress-development" || p.DeadlineSeconds < 60 || p.DeadlineSeconds > 1800 || p.RetentionSeconds < 60 || p.RetentionSeconds > 1800 {
		return fmt.Errorf("unsupported local development worker profile")
	}
	if p.BaseImage != BaseImage || p.WorkerRecipe != workerLabel || p.Sandbox != (Sandbox{MemoryMiB: 3072, WorkspaceMiB: 1536, TemporaryMiB: 64, PIDs: 256, CPUs: 2, FileMiB: 512}) || len(p.MavenGoals) != 3 || !slices.Equal(p.MavenGoals["package"], []string{"clean", "package"}) || !slices.Equal(p.MavenGoals["focused_tests"], []string{"clean", "test"}) || !slices.Equal(p.MavenGoals["verify"], []string{"clean", "verify"}) {
		return fmt.Errorf("profile recipe, commands, or sandbox differs from the implemented worker")
	}
	ids := p.ExpectedTestIDs()
	if len(ids) != 121 {
		return fmt.Errorf("pinned baseline inventory must contain 121 tests, got %d", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !testIDPattern.MatchString(id) || seen[id] {
			return fmt.Errorf("invalid baseline test inventory")
		}
		seen[id] = true
	}
	return nil
}

func (p Profile) ExpectedTestIDs() []string {
	var ids []string
	for class, methods := range p.BaselineTests {
		for _, method := range methods {
			ids = append(ids, class+"#"+method)
		}
	}
	sort.Strings(ids)
	return ids
}

// Reject duplicate and case-folded keys as well as unknown fields. A digest must
// identify one JSON interpretation, including at the HTTP upload boundary.
func strictJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var scan func(int) error
	scan = func(depth int) error {
		if depth > 32 {
			return fmt.Errorf("JSON nesting exceeds limit")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					s := strings.ToLower(key.(string))
					if seen[s] {
						return fmt.Errorf("duplicate JSON key")
					}
					seen[s] = true
					if err := scan(depth + 1); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := scan(depth + 1); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("invalid JSON delimiter")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := scan(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON content")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	// encoding/json accepts case-insensitive struct field aliases; reject them.
	canonical, err := json.Marshal(out)
	if err != nil {
		return err
	}
	var actual, expected any
	if err = json.Unmarshal(raw, &actual); err != nil {
		return err
	}
	if err = json.Unmarshal(canonical, &expected); err != nil {
		return err
	}
	var keys func(any, any) bool
	keys = func(a, b any) bool {
		switch x := a.(type) {
		case map[string]any:
			y, ok := b.(map[string]any)
			if !ok {
				return false
			}
			for k, v := range x {
				other, ok := y[k]
				if !ok || !keys(v, other) {
					return false
				}
			}
		case []any:
			y, ok := b.([]any)
			if !ok || len(x) != len(y) {
				return false
			}
			for i := range x {
				if !keys(x[i], y[i]) {
					return false
				}
			}
		}
		return true
	}
	if !keys(actual, expected) {
		return fmt.Errorf("JSON keys must use exact schema spelling")
	}
	return nil
}
