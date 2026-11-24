package localworker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
)

var commitSHAPattern = regexp.MustCompile(`^(?:[A-Fa-f0-9]{40}|[A-Fa-f0-9]{64})$`)

const (
	SourceURL    = "https://github.com/lzspangler/spring-boot-example-application"
	SourceCommit = "83b6f931c08071133678284548ff19b13f6f8db0"
	DefaultImage = "localhost/atg-java17-maven-v3:local"
	workerLabel  = "java17-maven-v3"
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

// ContextDiscovery selects conventional repository source roots, not individual files.
type ContextDiscovery struct {
	SourceRoots      []string `json:"source_roots"`
	UnitTestRoots    []string `json:"unit_test_roots"`
	SourceExtensions []string `json:"source_extensions"`
	UnitTestSuffix   string   `json:"unit_test_suffix"`
	BuildFiles       []string `json:"build_files"`
	MaxFiles         int      `json:"max_files"`
}

// ExecutionPair pins two immutable revisions of the profile repository. Base is
// the fixless subject; candidate is the revision a differential run must accept.
type ExecutionPair struct {
	BaseCommit      string `json:"base_commit"`
	CandidateCommit string `json:"candidate_commit"`
}

type Profile struct {
	SchemaVersion              string              `json:"schema_version"`
	ID                         string              `json:"id"`
	Version                    string              `json:"version"`
	RepositoryURL              string              `json:"repository_url"`
	Commit                     string              `json:"commit"`
	Subdirectory               string              `json:"subdirectory"`
	NetworkPolicy              string              `json:"network_policy"`
	BaseImage                  string              `json:"base_image"`
	WorkerRecipe               string              `json:"worker_recipe"`
	TestScope                  string              `json:"test_scope,omitempty"`
	TestNetworkMode            string              `json:"test_network_mode,omitempty"`
	DependencyPreparationGoals []string            `json:"dependency_preparation_goals,omitempty"`
	SurefireProvider           string              `json:"surefire_provider,omitempty"`
	ContextDiscovery           *ContextDiscovery   `json:"context_discovery,omitempty"`
	ExecutionPair              *ExecutionPair      `json:"execution_pair,omitempty"`
	Sandbox                    Sandbox             `json:"sandbox"`
	MavenGoals                 map[string][]string `json:"maven_goals"`
	DeadlineSeconds            int                 `json:"deadline_seconds"`
	RetentionSeconds           int                 `json:"retention_seconds"`
	BaselineTests              map[string][]string `json:"baseline_tests"`
	Digest                     string              `json:"-"`
	raw                        []byte
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
	if p.SchemaVersion != "1" || p.ID != "ssc-java17-maven" || (p.Version != "1.0.2" && p.Version != "1.0.4" && p.Version != "1.0.5" && p.Version != "1.0.6" && p.Version != "1.0.7" && p.Version != "1.0.8") || p.RepositoryURL != SourceURL && !(p.Version == "1.0.8" && p.ExecutionPair != nil && validRepositoryURL(p.RepositoryURL)) || p.Commit != SourceCommit || p.Subdirectory != "." || p.NetworkPolicy != "public-egress-development" || p.DeadlineSeconds < 60 || p.DeadlineSeconds > 1800 || p.RetentionSeconds < 60 || p.RetentionSeconds > 1800 {
		return fmt.Errorf("unsupported local development worker profile")
	}
	if p.BaseImage != BaseImage || p.Sandbox != (Sandbox{MemoryMiB: 3072, WorkspaceMiB: 1536, TemporaryMiB: 64, PIDs: 256, CPUs: 2, FileMiB: 512}) || len(p.MavenGoals) != 3 {
		return fmt.Errorf("profile recipe, commands, or sandbox differs from the implemented worker")
	}
	ids := p.ExpectedTestIDs()
	switch p.Version {
	case "1.0.2":
		if p.TestScope != "" || p.TestNetworkMode != "" || len(p.DependencyPreparationGoals) != 0 || p.SurefireProvider != "" || p.ContextDiscovery != nil || p.ExecutionPair != nil || p.WorkerRecipe != workerLabel || !slices.Equal(p.MavenGoals["package"], []string{"clean", "package"}) || !slices.Equal(p.MavenGoals["focused_tests"], []string{"clean", "test"}) || !slices.Equal(p.MavenGoals["verify"], []string{"clean", "verify"}) || len(ids) != 125 {
			return fmt.Errorf("version 1.0.2 must retain the 125-test package profile")
		}
	case "1.0.4", "1.0.5", "1.0.6", "1.0.7", "1.0.8":
		discoveryValid := p.Version == "1.0.4" && p.ContextDiscovery == nil || p.Version == "1.0.5" && p.ContextDiscovery != nil && p.ContextDiscovery.Validate() == nil && p.ContextDiscovery.MaxFiles <= 64 || (p.Version == "1.0.6" || p.Version == "1.0.7" || p.Version == "1.0.8") && p.ContextDiscovery != nil && p.ContextDiscovery.Validate() == nil
		pairRequired := p.Version == "1.0.7" || p.Version == "1.0.8"
		pairValid := !pairRequired && p.ExecutionPair == nil || pairRequired && p.ExecutionPair != nil && p.ExecutionPair.BaseCommit == p.Commit && commitSHAPattern.MatchString(p.ExecutionPair.CandidateCommit) && p.ExecutionPair.CandidateCommit != p.ExecutionPair.BaseCommit
		if p.TestScope != "unit" || !discoveryValid || !pairValid || (p.TestNetworkMode != "offline" && p.TestNetworkMode != "online") || !slices.Equal(p.DependencyPreparationGoals, []string{"org.apache.maven.plugins:maven-dependency-plugin:3.6.1:go-offline", "org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve", "org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve-plugins", "org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get"}) || p.SurefireProvider != "org.apache.maven.surefire:surefire-junit-platform:2.22.2" || p.WorkerRecipe != workerLabel || !slices.Equal(p.MavenGoals["package"], []string{"clean", "test"}) || !slices.Equal(p.MavenGoals["focused_tests"], []string{"clean", "test"}) || !slices.Equal(p.MavenGoals["verify"], []string{"clean", "test"}) || len(p.BaselineTests) != 1 || !slices.Equal(p.BaselineTests["com.redhat.lightwell.service.StatementImportServiceTest"], []string{"shouldImportDepositsAndWithdrawals", "shouldThrowNotFoundWhenAccountMissing", "shouldThrowRuntimeExceptionForMalformedXml", "shouldHandleEmptyStatement", "shouldSkipTransactionsWithMissingType", "shouldSkipTransactionsWithMissingAmount", "shouldUseDefaultDescriptionWhenMissing"}) {
			return fmt.Errorf("unit profile must have an explicit network mode, valid source-discovery rules, and a valid execution pair for its version")
		}
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

func (d *ContextDiscovery) Validate() error {
	if d == nil || len(d.SourceRoots) < 1 || len(d.SourceRoots) > 8 || len(d.UnitTestRoots) < 1 || len(d.UnitTestRoots) > 8 || len(d.SourceExtensions) < 1 || len(d.SourceExtensions) > 8 || len(d.BuildFiles) < 1 || len(d.BuildFiles) > 8 || d.MaxFiles <= len(d.BuildFiles) || d.MaxFiles > 128 || !validDiscoverySuffix(d.UnitTestSuffix) {
		return fmt.Errorf("invalid source-discovery bounds")
	}
	seen := map[string]bool{}
	for _, name := range append(slices.Clone(d.SourceRoots), d.UnitTestRoots...) {
		if !validProfilePath(name) || seen[name] {
			return fmt.Errorf("source-discovery roots must be unique normalized paths")
		}
		seen[name] = true
	}
	roots := make([]string, 0, len(seen))
	for root := range seen {
		roots = append(roots, root)
	}
	for i, root := range roots {
		for j, other := range roots {
			if i != j && strings.HasPrefix(other, root+"/") {
				return fmt.Errorf("source-discovery roots cannot overlap")
			}
		}
	}
	for _, ext := range d.SourceExtensions {
		if !validSourceExtension(ext) || seen[ext] {
			return fmt.Errorf("invalid source-discovery extension")
		}
		seen[ext] = true
	}
	suffixMatchesExtension := false
	for _, ext := range d.SourceExtensions {
		suffixMatchesExtension = suffixMatchesExtension || strings.HasSuffix(d.UnitTestSuffix, ext)
	}
	if !suffixMatchesExtension {
		return fmt.Errorf("unit-test suffix must match an allowed source extension")
	}
	for _, name := range d.BuildFiles {
		if !validProfilePath(name) || seen[name] {
			return fmt.Errorf("build metadata paths must be unique normalized paths")
		}
		seen[name] = true
	}
	return nil
}

func validProfilePath(value string) bool {
	if value == "" || len(value) > 240 || value != strings.TrimSpace(value) || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validSourceExtension(value string) bool {
	if len(value) < 2 || value[0] != '.' {
		return false
	}
	for _, r := range value[1:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// executionSubject names one immutable revision a Maven phase may execute
// against. A profile without a pair has the single pinned base subject.
type executionSubject struct {
	name   string
	commit string
}

func (p Profile) executionSubjects() []executionSubject {
	if p.ExecutionPair == nil {
		return []executionSubject{{name: "", commit: p.Commit}}
	}
	return []executionSubject{
		{name: "base", commit: p.ExecutionPair.BaseCommit},
		{name: "candidate", commit: p.ExecutionPair.CandidateCommit},
	}
}

// primarySubject is the subject single-subject phases (source, baseline)
// execute against: the pinned revision, named as the base under a pair.
func (p Profile) primarySubject() executionSubject {
	if p.ExecutionPair != nil {
		return executionSubject{name: "base", commit: p.ExecutionPair.BaseCommit}
	}
	return executionSubject{name: "", commit: p.Commit}
}

// validRepositoryURL accepts a credential-free https git remote, or a
// host-local http remote for development instances such as a local GitLab.
func validRepositoryURL(value string) bool {
	if len(value) > 512 || strings.ContainsAny(value, " \x00\r\n@") {
		return false
	}
	scheme, rest, _ := strings.Cut(value, "://")
	if scheme == "https" {
		host, _, ok := strings.Cut(rest, "/")
		return ok && host != "" && strings.Contains(host, ".")
	}
	if scheme == "http" {
		hostPort, _, _ := strings.Cut(rest, "/")
		host, _, _ := strings.Cut(hostPort, ":")
		return host == "host.containers.internal" || host == "localhost" || host == "127.0.0.1"
	}
	return false
}

func validDiscoverySuffix(value string) bool {
	if value == "" || len(value) > 64 || strings.ContainsAny(value, "/\\\x00\r\n") || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (p Profile) testNetworkMode() string {
	if p.TestScope != "unit" {
		return "online"
	}
	return p.TestNetworkMode
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

func (p Profile) testSelector(phase string, a TestArtifact) string {
	selected := map[string]map[string]bool{}
	addID := func(id string) {
		class, method, ok := strings.Cut(id, "#")
		if !ok || !testIDPattern.MatchString(id) {
			return
		}
		if selected[class] == nil {
			selected[class] = map[string]bool{}
		}
		selected[class][method] = true
	}
	if p.TestScope == "unit" && (phase == "package" || phase == "verify") {
		for _, id := range p.ExpectedTestIDs() {
			addID(id)
		}
	}
	if phase == "focused_tests" || p.TestScope == "unit" && phase == "verify" {
		for _, id := range a.ExpectedTestIDs {
			addID(id)
		}
	}
	if len(selected) == 0 {
		return ""
	}
	classes := make([]string, 0, len(selected))
	for class := range selected {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	selectors := make([]string, 0, len(classes))
	for _, class := range classes {
		methods := make([]string, 0, len(selected[class]))
		for method := range selected[class] {
			methods = append(methods, method)
		}
		sort.Strings(methods)
		selectors = append(selectors, class+"#"+strings.Join(methods, "+"))
	}
	return strings.Join(selectors, ",")
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
