package localworker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/artifact"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

const profilePath = "../../../../workers/java17-maven/profile.json"
const unitProfilePath = "../../../../workers/java17-maven/profile-unit.json"
const onlineUnitProfilePath = "../../../../workers/java17-maven/profile-unit-online.json"
const unitTestArtifact = `{
  "schema_version": "1",
  "files": [{"path":"src/test/java/com/redhat/lightwell/atg/ArithmeticSmokeTest.java","content":"package com.redhat.lightwell.atg;\nclass ArithmeticSmokeTest {}\n"}],
  "expected_test_ids": ["com.redhat.lightwell.atg.ArithmeticSmokeTest#addsWithoutOverflow"]
}`

// Unit tests use synthetic inventories; only explicit OCI integration tests read
// the independently versioned workload profile outside this module.
func unitProfile(t *testing.T) Profile {
	t.Helper()
	methods := make([]string, 125)
	for i := range methods {
		methods[i] = fmt.Sprintf("behavior%d", i)
	}
	p := Profile{
		SchemaVersion: "1", ID: "ssc-java17-maven", Version: "1.0.2", RepositoryURL: SourceURL,
		Commit: SourceCommit, Subdirectory: ".", NetworkPolicy: "public-egress-development",
		BaseImage: BaseImage, WorkerRecipe: workerLabel, RetentionSeconds: 1800, DeadlineSeconds: 600,
		Sandbox:       Sandbox{MemoryMiB: 3072, WorkspaceMiB: 1536, TemporaryMiB: 64, PIDs: 256, CPUs: 2, FileMiB: 512},
		MavenGoals:    map[string][]string{"package": {"clean", "package"}, "focused_tests": {"clean", "test"}, "verify": {"clean", "verify"}},
		BaselineTests: map[string][]string{"example.BaselineTest": methods},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p, err = DecodeProfile(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfiguredWorkloadProfileArtifact(t *testing.T) {
	path := os.Getenv("ATG_TEST_LOCALWORKER_PROFILE")
	if path == "" {
		t.Skip("set ATG_TEST_LOCALWORKER_PROFILE to validate the versioned workload profile artifact")
	}
	if _, err := LoadProfile(path); err != nil {
		t.Fatalf("invalid configured workload profile: %v", err)
	}
}

func TestUnitOnlyProfileSelectsOnlyTheApprovedTestClass(t *testing.T) {
	p, err := LoadProfile(unitProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	const baselineClass = "com.redhat.lightwell.service.StatementImportServiceTest"
	baselineSelector := baselineClass + "#shouldHandleEmptyStatement+shouldImportDepositsAndWithdrawals+shouldSkipTransactionsWithMissingAmount+shouldSkipTransactionsWithMissingType+shouldThrowNotFoundWhenAccountMissing+shouldThrowRuntimeExceptionForMalformedXml+shouldUseDefaultDescriptionWhenMissing"
	if p.Version != "1.0.6" || p.TestScope != "unit" || p.TestNetworkMode != "offline" || p.ContextDiscovery == nil || p.ContextDiscovery.MaxFiles != 128 || !slices.Equal(p.ContextDiscovery.SourceRoots, []string{"src/main/java"}) || !slices.Equal(p.ContextDiscovery.UnitTestRoots, []string{"src/test/java"}) || !slices.Equal(p.DependencyPreparationGoals, []string{"org.apache.maven.plugins:maven-dependency-plugin:3.6.1:go-offline", "org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve", "org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve-plugins", "org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get"}) || p.SurefireProvider != "org.apache.maven.surefire:surefire-junit-platform:2.22.2" || len(p.ExpectedTestIDs()) != 7 || p.testSelector("package", TestArtifact{}) != baselineSelector {
		t.Fatalf("unit profile does not select its explicit baseline: %#v", p)
	}
	generated := TestArtifact{Files: []TestFile{{Path: "src/test/java/com/redhat/lightwell/service/StatementImportSecurityTest.java"}}, ExpectedTestIDs: []string{
		"com.redhat.lightwell.service.StatementImportSecurityTest#doesNotExposeExternalData",
		"com.redhat.lightwell.service.StatementImportSecurityTest#rejectsMalformedInput",
	}}
	generatedSelector := "com.redhat.lightwell.service.StatementImportSecurityTest#doesNotExposeExternalData+rejectsMalformedInput"
	if got, want := p.testSelector("focused_tests", generated), generatedSelector; got != want {
		t.Fatalf("focused selector = %q, want %q", got, want)
	}
	if got, want := p.testSelector("verify", generated), generatedSelector+","+baselineSelector; got != want {
		t.Fatalf("final selector = %q, want %q", got, want)
	}
	if got := p.testSelector("verify", TestArtifact{}); got != baselineSelector {
		t.Fatalf("unit verify selector = %q", got)
	}
}

func TestUnitProfileNetworkModeIsExplicitAndDigestBound(t *testing.T) {
	offline, err := LoadProfile(unitProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadProfile(onlineUnitProfilePath)
	if err != nil || p.testNetworkMode() != "online" || p.Digest == offline.Digest {
		t.Fatalf("online mode was not accepted as a distinct profile identity: %v", err)
	}
	raw, err := os.ReadFile(onlineUnitProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(string(raw), `"test_network_mode": "online"`, `"test_network_mode": "unrestricted"`, 1)
	if _, err = DecodeProfile([]byte(invalid)); err == nil {
		t.Fatal("profile accepted an unrecognized test network mode")
	}
	worker := &Worker{profile: p}
	if args := worker.sandboxArgs("name", "run", "digest", "bridge", 120); !slices.Contains(args, "--env=ATG_TEST_NETWORK_MODE=online") || !slices.Contains(args, "--env=ATG_SUREFIRE_PROVIDER="+p.SurefireProvider) {
		t.Fatalf("online mode or test provider was not bound into the worker environment: %v", args)
	}
}

func TestUnitProfileRejectsBroadMavenGoal(t *testing.T) {
	raw, err := os.ReadFile(unitProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), `"package": ["clean", "test"]`, `"package": ["clean", "package"]`, 1)
	if changed == string(raw) {
		t.Fatal("test did not mutate the unit profile's package goal")
	}
	if _, err := DecodeProfile([]byte(changed)); err == nil {
		t.Fatal("unit-only profile accepted the broad package goal")
	}
}

const pairProfilePath = "../../../../workers/java17-maven/profile-unit-pair-online.json"
const cvePairProfilePath = "../../../../workers/java17-maven/profile-unit-pair-cve-online.json"

const pairCandidateCommit = "01a18a1b40e151ffcc1d7bef86bf335f87999750"

func pairProfile(t *testing.T) Profile {
	t.Helper()
	p := Profile{
		SchemaVersion: "1", ID: "ssc-java17-maven", Version: "1.0.7", RepositoryURL: SourceURL,
		Commit: SourceCommit, Subdirectory: ".", NetworkPolicy: "public-egress-development",
		BaseImage: BaseImage, WorkerRecipe: workerLabel, RetentionSeconds: 1800, DeadlineSeconds: 600,
		TestScope:       "unit",
		TestNetworkMode: "online",
		DependencyPreparationGoals: []string{
			"org.apache.maven.plugins:maven-dependency-plugin:3.6.1:go-offline",
			"org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve",
			"org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve-plugins",
			"org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get",
		},
		SurefireProvider: "org.apache.maven.surefire:surefire-junit-platform:2.22.2",
		ContextDiscovery: &ContextDiscovery{SourceRoots: []string{"src/main/java"}, UnitTestRoots: []string{"src/test/java"}, SourceExtensions: []string{".java"}, UnitTestSuffix: "Test.java", BuildFiles: []string{"pom.xml"}, MaxFiles: 128},
		ExecutionPair:    &ExecutionPair{BaseCommit: SourceCommit, CandidateCommit: pairCandidateCommit},
		Sandbox:          Sandbox{MemoryMiB: 3072, WorkspaceMiB: 1536, TemporaryMiB: 64, PIDs: 256, CPUs: 2, FileMiB: 512},
		MavenGoals:       map[string][]string{"package": {"clean", "test"}, "focused_tests": {"clean", "test"}, "verify": {"clean", "test"}},
		BaselineTests: map[string][]string{"com.redhat.lightwell.service.StatementImportServiceTest": {
			"shouldImportDepositsAndWithdrawals", "shouldThrowNotFoundWhenAccountMissing", "shouldThrowRuntimeExceptionForMalformedXml",
			"shouldHandleEmptyStatement", "shouldSkipTransactionsWithMissingType", "shouldSkipTransactionsWithMissingAmount", "shouldUseDefaultDescriptionWhenMissing"}},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p, err = DecodeProfile(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func pairFixture(t *testing.T) (*Worker, testgenflow.WorkRequest, *memoryStore) {
	t.Helper()
	p := pairProfile(t)
	s := &memoryStore{objects: map[string][]byte{}}
	w, err := New(Config{Profile: p, Artifacts: s})
	if err != nil {
		t.Fatal(err)
	}
	w.imageID = "sha256:" + strings.Repeat("a", 64)
	if _, err = s.Put(context.Background(), p.raw); err != nil {
		t.Fatal(err)
	}
	bound, err := s.Put(context.Background(), boundProfile(p, w.imageID))
	if err != nil {
		t.Fatal(err)
	}
	w.boundDigest.Store(bound.Digest)
	r := testgenflow.WorkRequest{RunID: "worker-pair-test-run", IdempotencyKey: "worker-pair-test-run:source", Request: testgenflow.Request{RepositoryURL: SourceURL, Revision: SourceCommit, BaseBranch: "main", Subdirectory: ".", ProfileID: p.ID, ProfileVersion: p.Version, ProfileDigest: w.ProfileDigest(), InputMode: "artifact_input", CompletionMode: "patch_only"}}
	return w, r, s
}

func TestPairProfilePinsTwoImmutableSubjects(t *testing.T) {
	p, err := LoadProfile(pairProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "1.0.7" || p.ExecutionPair == nil || p.ExecutionPair.BaseCommit != SourceCommit || p.ExecutionPair.CandidateCommit != pairCandidateCommit || p.TestNetworkMode != "online" {
		t.Fatalf("pair profile does not pin its immutable subjects: %#v", p)
	}
	raw, err := os.ReadFile(pairProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"candidate equals base":  func(b []byte) []byte { return bytes.Replace(b, []byte(pairCandidateCommit), []byte(SourceCommit), 1) },
		"candidate not a commit": func(b []byte) []byte { return bytes.Replace(b, []byte(pairCandidateCommit), []byte("not-a-commit"), 1) },
		"pair without version": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"version": "1.0.7"`), []byte(`"version": "1.0.6"`), 1)
		},
		"pair without base match": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"base_commit": "`+SourceCommit+`"`), []byte(`"base_commit": "`+pairCandidateCommit+`"`), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeProfile(mutate(raw)); err == nil {
				t.Fatal("pair profile accepted an invalid execution pair")
			}
		})
	}
	online, err := os.ReadFile(onlineUnitProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	withPair := bytes.Replace(online, []byte(`"test_network_mode": "online",`), []byte(`"test_network_mode": "online", "execution_pair": {"base_commit": "`+SourceCommit+`", "candidate_commit": "`+pairCandidateCommit+`"},`), 1)
	if _, err := DecodeProfile(withPair); err == nil {
		t.Fatal("non-pair profile version accepted an execution pair")
	}
}

func TestCVEPairProfilePinsForkRepositoryAndFixCandidate(t *testing.T) {
	p, err := LoadProfile(cvePairProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "1.0.8" || p.RepositoryURL != "http://host.containers.internal:8090/development/spring-boot-example-application.git" || p.ExecutionPair.BaseCommit != SourceCommit || p.ExecutionPair.CandidateCommit != "518440bd30e5a2b5760861e7709e83963461fe8f" {
		t.Fatalf("local CVE pair profile did not retain its pins: %#v", p)
	}
	raw, err := os.ReadFile(cvePairProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"remote http host": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`http://host.containers.internal:8090`), []byte(`http://git.example`), 1)
		},
		"credential url": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`http://host.containers.internal:8090`), []byte(`http://token@host.containers.internal:8090`), 1)
		},
		"pair on old version": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"version": "1.0.8"`), []byte(`"version": "1.0.7"`), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeProfile(mutate(raw)); err == nil {
				t.Fatal("local CVE pair profile accepted an invalid remote or version combination")
			}
		})
	}
	fork := bytes.Replace(raw,
		[]byte(`http://host.containers.internal:8090/development/spring-boot-example-application.git`),
		[]byte(`https://github.com/example/spring-boot-example-application`), 1)
	if p, err = DecodeProfile(fork); err != nil || p.RepositoryURL != "https://github.com/example/spring-boot-example-application" {
		t.Fatalf("https fork remote rejected for a v1.0.8 pair profile: %v", err)
	}
}

func TestCVEPairProfilePinsForkRepositoryAndFixCandidateLegacy(t *testing.T) {
	raw, err := os.ReadFile(pairProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	fork := bytes.Replace(raw,
		[]byte(`"version": "1.0.7"`),
		[]byte(`"version": "1.0.8"`), 1)
	fork = bytes.Replace(fork,
		[]byte(`"repository_url": "https://github.com/lzspangler/spring-boot-example-application"`),
		[]byte(`"repository_url": "https://github.com/example/spring-boot-example-application"`), 1)
	fork = bytes.Replace(fork,
		[]byte(pairCandidateCommit),
		[]byte("518440bd30e5a2b5760861e7709e83963461fe8f"), 1)
	p, err := DecodeProfile(fork)
	if err != nil {
		t.Fatalf("CVE fix pair profile rejected: %v", err)
	}
	if p.Version != "1.0.8" || p.RepositoryURL != "https://github.com/example/spring-boot-example-application" || p.ExecutionPair.CandidateCommit != "518440bd30e5a2b5760861e7709e83963461fe8f" {
		t.Fatalf("CVE fix pair profile did not retain its pins: %#v", p)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"fork url on old version": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"version": "1.0.8"`), []byte(`"version": "1.0.7"`), 1)
		},
		"fork url without pair": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"test_network_mode": "online",`), []byte(`"test_network_mode": "online", "execution_pair": null,`), 1)
		},
		"credential url": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`https://github.com/example/spring-boot-example-application`), []byte(`https://token@github.com/example/spring-boot-example-application`), 1)
		},
		"non https url": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`https://github.com/example/spring-boot-example-application`), []byte(`http://github.com/example/spring-boot-example-application`), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeProfile(mutate(fork)); err == nil {
				t.Fatal("CVE fix pair profile accepted an invalid fork or version combination")
			}
		})
	}
}

type pairJob struct {
	state   containerState
	files   map[string][]byte
	status  string
	receipt string
}

type pairEngine struct {
	t               *testing.T
	jobs            map[string]*pairJob
	filesBySubject  map[string]map[string][]byte
	statusBySubject map[string]string
	calls           [][]string
	creates         int
}

func (f *pairEngine) run(ctx context.Context, stdin []byte, _ int64, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.calls = append(f.calls, slices.Clone(args))
	key := strings.Join(args, " ")
	execID := args[1]
	if args[0] == "exec" && args[1] == "-i" {
		execID = args[2]
	}
	switch {
	case strings.HasPrefix(key, "container exists "):
		if _, ok := f.jobs[args[2]]; !ok {
			return nil, &engineError{code: 1}
		}
		return nil, nil
	case strings.HasPrefix(key, "container inspect "):
		job, ok := f.jobs[args[2]]
		if !ok {
			return json.Marshal([]containerState{})
		}
		return json.Marshal([]containerState{job.state})
	case args[0] == "create":
		f.creates++
		name := args[2]
		subject := ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "--label=io.lightpath.atg.subject=") {
				subject = strings.TrimPrefix(arg, "--label=io.lightpath.atg.subject=")
			}
		}
		job := &pairJob{files: f.filesBySubject[subject], status: f.statusBySubject[subject]}
		job.state = containerState{ID: name, Image: args[len(args)-6]}
		job.state.State.Status = "created"
		job.state.Config.Labels = map[string]string{}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--label=") {
				parts := strings.SplitN(strings.TrimPrefix(arg, "--label="), "=", 2)
				job.state.Config.Labels[parts[0]] = parts[1]
			}
		}
		f.jobs[name] = job
		return []byte(job.state.ID), nil
	case args[0] == "start":
		job := f.jobs[args[1]]
		job.state.State.Status = "running"
		job.state.State.Running = true
		job.state.State.StartedAt = time.Now()
		return []byte(job.state.ID), nil
	case args[0] == "exec" && args[len(args)-1] == "status":
		job := f.jobs[execID]
		if job.status != "" {
			return []byte(job.status), nil
		}
		return []byte("0\n"), nil
	case args[0] == "exec" && (args[len(args)-1] == "collect" || args[len(args)-1] == "partial"):
		return bundle(f.t, f.jobs[execID].files), nil
	case args[0] == "exec" && (args[len(args)-1] == "seal" || args[len(args)-1] == "cancel"):
		job := f.jobs[execID]
		job.receipt = strings.TrimSpace(string(stdin))
		job.state.State.Running = false
		job.state.State.Status = "exited"
		_ = json.Unmarshal(job.files["exit"], &job.state.State.ExitCode)
		return nil, nil
	case args[0] == "logs":
		return []byte("ATG_RECEIPT=" + f.jobs[args[1]].receipt + "\n"), nil
	case args[0] == "ps":
		rows := []map[string]string{}
		for _, job := range f.jobs {
			rows = append(rows, map[string]string{"Id": job.state.ID})
		}
		return json.Marshal(rows)
	case args[0] == "rm":
		delete(f.jobs, args[1])
		return nil, nil
	case args[0] == "stop":
		job := f.jobs[args[3]]
		job.state.State.Running = false
		job.state.State.Status = "exited"
		job.state.State.ExitCode = 137
		return nil, nil
	default:
		f.t.Fatalf("unexpected engine command %v", args)
		return nil, nil
	}
}

func pairSubjectFiles(commit string, failing bool, expected []string) map[string][]byte {
	var xml strings.Builder
	xml.WriteString("<testsuite>")
	for i, id := range expected {
		parts := strings.Split(id, "#")
		fmt.Fprintf(&xml, `<testcase classname="%s" name="%s">`, parts[0], parts[1])
		if failing && i == 0 {
			xml.WriteString("<failure/>")
		}
		xml.WriteString("</testcase>")
	}
	xml.WriteString("</testsuite>")
	exit := 0
	if failing {
		exit = 1
	}
	return map[string][]byte{
		"exit":                 []byte(fmt.Sprintf("%d\n", exit)),
		"source-identity":      []byte(commit + "\ntree\n"),
		"source.tar.gz":        []byte("source snapshot"),
		"toolchain":            []byte("java17 maven"),
		"command.log":          []byte("maven run"),
		"reports/TEST-all.xml": []byte(xml.String()),
	}
}

func TestPairProfileSourceSealsAsBaseSubject(t *testing.T) {
	w, r, _ := pairFixture(t)
	f := &pairEngine{
		t: t, jobs: map[string]*pairJob{},
		filesBySubject: map[string]map[string][]byte{
			"base": {"exit": []byte("0\n"), "source-identity": []byte(SourceCommit + "\ntree\n"), "source.tar.gz": []byte("source snapshot"), "toolchain": []byte("java17 maven"), "command.log": []byte("source checkout")},
		},
	}
	w.exec = f.run
	commit, err := w.ResolveSource(context.Background(), r)
	if err != nil || commit != SourceCommit {
		t.Fatalf("pair-profile source resolution: %q %v", commit, err)
	}
	for _, call := range f.calls {
		if call[0] == "create" && call[len(call)-1] != SourceCommit {
			t.Fatalf("source container not pinned to the base commit: %v", call)
		}
	}
	refs, err := w.RunEvidence(context.Background(), r.RunID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("source receipt under pair profile: %v %v", refs, err)
	}
}

func TestPairFocusedExecutionRecordsBothSubjects(t *testing.T) {
	w, r, store := pairFixture(t)
	raw := []byte(unitTestArtifact)
	ref, err := store.Put(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	r.Request.InputArtifactDigest = ref.Digest
	generated, err := w.PrepareTestArtifact(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.TestArtifactDigest = generated.TestArtifactDigest
	r.PatchDigest = generated.PatchDigest
	r.ChangedTestPaths = generated.ChangedTestPaths
	a, err := DecodeTestArtifact(raw)
	if err != nil {
		t.Fatal(err)
	}
	f := &pairEngine{
		t: t, jobs: map[string]*pairJob{},
		filesBySubject: map[string]map[string][]byte{
			"base":      pairSubjectFiles(SourceCommit, true, a.ExpectedTestIDs),
			"candidate": pairSubjectFiles(pairCandidateCommit, false, a.ExpectedTestIDs),
		},
		statusBySubject: map[string]string{"base": "1\n"},
	}
	w.exec = f.run
	result, err := w.RunMaven(context.Background(), testgenflow.MavenWorkRequest{WorkRequest: r, Phase: testgenflow.MavenPhaseFocusedTests})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded || result.EvidenceRef == "" {
		t.Fatalf("candidate-passing pair must succeed: %+v", result)
	}
	if f.creates != 2 {
		t.Fatalf("expected one container per subject, created %d", f.creates)
	}
	commits := map[string]bool{}
	for _, call := range f.calls {
		if call[0] == "create" {
			commits[call[len(call)-1]] = true
		}
	}
	if !commits[SourceCommit] || !commits[pairCandidateCommit] {
		t.Fatalf("subject containers were not pinned to both pair commits: %v", commits)
	}
	document, err := w.get(context.Background(), result.EvidenceRef, MaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	var pair struct {
		Phase    string `json:"phase"`
		Subjects []struct {
			Subject     string `json:"subject"`
			Commit      string `json:"commit"`
			Succeeded   bool   `json:"succeeded"`
			EvidenceRef string `json:"evidence_ref"`
		} `json:"subjects"`
		Succeeded            bool `json:"succeeded"`
		DifferentialObserved bool `json:"differential_observed"`
	}
	if err = json.Unmarshal(document, &pair); err != nil || pair.Phase != "focused_tests" || len(pair.Subjects) != 2 {
		t.Fatalf("pair document: %s %v", document, err)
	}
	base, candidate := pair.Subjects[0], pair.Subjects[1]
	if base.Subject != "base" || base.Commit != SourceCommit || base.Succeeded {
		t.Fatalf("base subject outcome not recorded: %+v", base)
	}
	if candidate.Subject != "candidate" || candidate.Commit != pairCandidateCommit || !candidate.Succeeded {
		t.Fatalf("candidate subject outcome not recorded: %+v", candidate)
	}
	if !pair.Succeeded || !pair.DifferentialObserved {
		t.Fatalf("differential observation not derived: %+v", pair)
	}
	for _, subject := range pair.Subjects {
		subjectEvidence, err := w.get(context.Background(), subject.EvidenceRef, MaxEvidenceBytes)
		if err != nil {
			t.Fatal(err)
		}
		var evidence Evidence
		if err = strictJSON(subjectEvidence, &evidence); err != nil || evidence.Subject != subject.Subject || evidence.SourceCommit != subject.Commit || evidence.Phase != "focused_tests" {
			t.Fatalf("subject evidence not bound to its immutable revision: %s", subjectEvidence)
		}
	}
	refs, err := w.RunEvidence(context.Background(), r.RunID)
	if err != nil || len(refs) != 2 {
		t.Fatalf("retained per-subject receipts %v %v", refs, err)
	}
	if err = w.CleanupRun(context.Background(), r.RunID); err != nil || len(f.jobs) != 0 {
		t.Fatalf("cleanup failed: %v jobs=%d", err, len(f.jobs))
	}
}

func TestPairCandidateFailureFailsThePhase(t *testing.T) {
	w, r, store := pairFixture(t)
	raw := []byte(unitTestArtifact)
	ref, err := store.Put(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	r.Request.InputArtifactDigest = ref.Digest
	generated, err := w.PrepareTestArtifact(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.TestArtifactDigest = generated.TestArtifactDigest
	r.PatchDigest = generated.PatchDigest
	r.ChangedTestPaths = generated.ChangedTestPaths
	a, err := DecodeTestArtifact(raw)
	if err != nil {
		t.Fatal(err)
	}
	f := &pairEngine{
		t: t, jobs: map[string]*pairJob{},
		filesBySubject: map[string]map[string][]byte{
			"base":      pairSubjectFiles(SourceCommit, false, a.ExpectedTestIDs),
			"candidate": pairSubjectFiles(pairCandidateCommit, true, a.ExpectedTestIDs),
		},
		statusBySubject: map[string]string{"candidate": "1\n"},
	}
	w.exec = f.run
	result, err := w.RunMaven(context.Background(), testgenflow.MavenWorkRequest{WorkRequest: r, Phase: testgenflow.MavenPhaseFocusedTests})
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded || result.Diagnostic == "" {
		t.Fatalf("candidate failure must fail the phase: %+v", result)
	}
	document, err := w.get(context.Background(), result.EvidenceRef, MaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	var pair struct {
		Subjects []struct {
			Subject   string `json:"subject"`
			Succeeded bool   `json:"succeeded"`
		} `json:"subjects"`
		DifferentialObserved bool `json:"differential_observed"`
	}
	if err = json.Unmarshal(document, &pair); err != nil || len(pair.Subjects) != 2 || !pair.Subjects[0].Succeeded || pair.Subjects[1].Succeeded || pair.DifferentialObserved {
		t.Fatalf("candidate-failure pair document: %s %v", document, err)
	}
}

func TestContextDiscoveryRulesRejectUnsafeOrUnboundedSelections(t *testing.T) {
	base := ContextDiscovery{SourceRoots: []string{"src/main/java"}, UnitTestRoots: []string{"src/test/java"}, SourceExtensions: []string{".java"}, UnitTestSuffix: "Test.java", BuildFiles: []string{"pom.xml"}, MaxFiles: 128}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid conventional source discovery rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ContextDiscovery){
		"source traversal":   func(d *ContextDiscovery) { d.SourceRoots[0] = "../src/main/java" },
		"overlapping roots":  func(d *ContextDiscovery) { d.UnitTestRoots[0] = "src/main/java/test" },
		"wildcard extension": func(d *ContextDiscovery) { d.SourceExtensions[0] = ".*" },
		"suffix path":        func(d *ContextDiscovery) { d.UnitTestSuffix = "nested/Test.java" },
		"build traversal":    func(d *ContextDiscovery) { d.BuildFiles[0] = "../pom.xml" },
		"too many files":     func(d *ContextDiscovery) { d.MaxFiles = 129 },
	} {
		t.Run(name, func(t *testing.T) {
			d := base
			d.SourceRoots = slices.Clone(base.SourceRoots)
			d.UnitTestRoots = slices.Clone(base.UnitTestRoots)
			d.SourceExtensions = slices.Clone(base.SourceExtensions)
			d.BuildFiles = slices.Clone(base.BuildFiles)
			mutate(&d)
			if err := d.Validate(); err == nil {
				t.Fatal("unsafe discovery rules accepted")
			}
		})
	}
}

func TestOCIOfflineReadinessProbe(t *testing.T) {
	if os.Getenv("ATG_TEST_LOCALWORKER_OCI") != "1" {
		t.Skip("set ATG_TEST_LOCALWORKER_OCI=1 for rootless OCI network-namespace probe")
	}
	p, err := LoadProfile(unitProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.NewFileStore(t.TempDir(), MaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := New(Config{Profile: p, Artifacts: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = w.Ready(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOCIUnitProfileRunsExactBaseline(t *testing.T) {
	if os.Getenv("ATG_TEST_LOCALWORKER_OCI") != "1" {
		t.Skip("set ATG_TEST_LOCALWORKER_OCI=1 for rootless OCI Maven check")
	}
	profilePath := unitProfilePath
	if os.Getenv("ATG_TEST_LOCALWORKER_MODE") == "online" {
		profilePath = onlineUnitProfilePath
	}
	p, err := LoadProfile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.NewFileStore(t.TempDir(), MaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := New(Config{Profile: p, Artifacts: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	if err = w.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	_, r, _ := fixture(t)
	r.Request.ProfileVersion = p.Version
	r.Request.ProfileDigest = w.ProfileDigest()
	r.RunID = fmt.Sprintf("oci-unit-baseline-%d", time.Now().UnixNano())
	r.IdempotencyKey = r.RunID + ":baseline"
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = w.CancelRun(cleanupCtx, r.RunID)
		if err := w.CleanupRun(cleanupCtx, r.RunID); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}()
	result, err := w.RunMaven(ctx, testgenflow.MavenWorkRequest{WorkRequest: r, Phase: testgenflow.MavenPhasePackage})
	if err != nil {
		t.Fatalf("baseline operation: %v", err)
	}
	raw, err := w.get(ctx, result.EvidenceRef, MaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	var evidence Evidence
	if err = strictJSON(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded {
		log := []byte("command log unavailable")
		if ref := evidence.Artifacts["command.log"]; ref != "" {
			log, _ = w.get(ctx, ref, MaxEvidenceBytes)
		}
		if len(log) > 32*1024 {
			log = log[len(log)-32*1024:]
		}
		t.Fatalf("baseline failed: diagnostic=%q summary=%#v log=%s", evidence.Diagnostic, evidence.Summary, log)
	}
	want := p.ExpectedTestIDs()
	if evidence.ProfileDigest != w.ProfileDigest() || !slices.Equal(evidence.ExpectedTestIDs, want) || !slices.Equal(evidence.Summary.ExecutedTestIDs, want) || len(evidence.Summary.SkippedTestIDs) != 0 || len(evidence.Summary.FailedTestIDs) != 0 || len(evidence.Summary.UnexpectedTestIDs) != 0 {
		t.Fatalf("baseline evidence differs from explicit inventory: %#v", evidence.Summary)
	}
}

type memoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	failGet bool
}

func (s *memoryStore) Put(_ context.Context, b []byte) (artifact.Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := artifact.Digest(b)
	s.objects[d] = append([]byte(nil), b...)
	return artifact.Ref{Digest: d, SizeBytes: int64(len(b))}, nil
}
func (s *memoryStore) Get(_ context.Context, d string, _ int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGet {
		return nil, fmt.Errorf("readback failure")
	}
	b, ok := s.objects[d]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), b...), nil
}

func fixture(t *testing.T) (*Worker, testgenflow.WorkRequest, *memoryStore) {
	t.Helper()
	p := unitProfile(t)
	s := &memoryStore{objects: map[string][]byte{}}
	w, err := New(Config{Profile: p, Artifacts: s})
	if err != nil {
		t.Fatal(err)
	}
	w.imageID = "sha256:" + strings.Repeat("a", 64)
	if _, err = s.Put(context.Background(), p.raw); err != nil {
		t.Fatal(err)
	}
	bound, err := s.Put(context.Background(), boundProfile(p, w.imageID))
	if err != nil {
		t.Fatal(err)
	}
	w.boundDigest.Store(bound.Digest)
	r := testgenflow.WorkRequest{RunID: "worker-test-run", IdempotencyKey: "worker-test-run:source", Request: testgenflow.Request{RepositoryURL: SourceURL, Revision: SourceCommit, BaseBranch: "main", Subdirectory: ".", ProfileID: p.ID, ProfileVersion: p.Version, ProfileDigest: w.ProfileDigest(), InputMode: "artifact_input", CompletionMode: "patch_only"}}
	return w, r, s
}

func bundle(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type fakeEngine struct {
	name                                               string
	t                                                  *testing.T
	state                                              containerState
	exists                                             bool
	creates, starts, removes, collects                 int
	receipt                                            string
	files                                              map[string][]byte
	createLost, startLost, sealLost, inspectionFailure bool
	status                                             string
	calls                                              [][]string
}

func (f *fakeEngine) run(ctx context.Context, stdin []byte, _ int64, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.calls = append(f.calls, slices.Clone(args))
	key := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(key, "container exists "):
		if f.inspectionFailure {
			return nil, &engineError{code: 125, cause: errors.New("transport")}
		}
		if !f.exists || (f.name != "" && args[2] != f.name && args[2] != f.state.ID) {
			return nil, &engineError{code: 1}
		}
		return nil, nil
	case strings.HasPrefix(key, "container inspect "):
		return json.Marshal([]containerState{f.state})
	case args[0] == "create":
		f.creates++
		f.name = args[2]
		f.exists = true
		f.state = containerState{ID: strings.Repeat("b", 64), Image: args[len(args)-6]}
		f.state.State.Status = "created"
		f.state.Config.Labels = map[string]string{}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--label=") {
				parts := strings.SplitN(strings.TrimPrefix(arg, "--label="), "=", 2)
				f.state.Config.Labels[parts[0]] = parts[1]
			}
		}
		if f.createLost {
			return nil, errors.New("lost create ack")
		}
		return []byte(f.state.ID), nil
	case args[0] == "start":
		f.starts++
		f.state.State.Status = "running"
		f.state.State.Running = true
		f.state.State.StartedAt = time.Now()
		if f.startLost {
			return nil, errors.New("lost start ack")
		}
		return []byte(f.state.ID), nil
	case args[0] == "exec" && args[len(args)-1] == "status":
		if f.status != "" {
			return []byte(f.status), nil
		}
		return []byte("0\n"), nil
	case args[0] == "exec" && (args[len(args)-1] == "collect" || args[len(args)-1] == "partial"):
		f.collects++
		return bundle(f.t, f.files), nil
	case args[0] == "exec" && (args[len(args)-1] == "seal" || args[len(args)-1] == "cancel"):
		f.receipt = strings.TrimSpace(string(stdin))
		f.state.State.Running = false
		f.state.State.Status = "exited"
		_ = json.Unmarshal(f.files["exit"], &f.state.State.ExitCode)
		if args[len(args)-1] == "cancel" {
			f.state.State.ExitCode = 130
		}
		if f.sealLost {
			return nil, errors.New("lost seal ack")
		}
		return nil, nil
	case args[0] == "logs":
		return []byte("ATG_RECEIPT=" + f.receipt + "\n"), nil
	case args[0] == "ps":
		if f.exists {
			return json.Marshal([]map[string]string{{"Id": f.state.ID}})
		}
		return []byte("[]"), nil
	case args[0] == "rm":
		f.removes++
		f.exists = false
		return nil, nil
	case args[0] == "stop":
		f.state.State.Running = false
		f.state.State.Status = "exited"
		f.state.State.ExitCode = 137
		return nil, nil
	default:
		f.t.Fatalf("unexpected engine command %v", args)
		return nil, nil
	}
}

func sourceFiles() map[string][]byte {
	return map[string][]byte{"exit": []byte("0\n"), "source-identity": []byte(SourceCommit + "\ntree\n"), "source.tar.gz": []byte("source snapshot"), "toolchain": []byte("java17 maven"), "command.log": []byte("source checkout")}
}

func TestSourceLostAcknowledgementsReconcileSameWorker(t *testing.T) {
	for _, fault := range []string{"none", "create", "start", "seal"} {
		t.Run(fault, func(t *testing.T) {
			w, r, store := fixture(t)
			f := &fakeEngine{t: t, files: sourceFiles(), createLost: fault == "create", startLost: fault == "start", sealLost: fault == "seal"}
			w.exec = f.run
			commit, err := w.ResolveSource(context.Background(), r)
			if err != nil || commit != SourceCommit {
				t.Fatalf("resolve: %q %v", commit, err)
			}
			// Reconstruct adapter with no memory of the previous call.
			w2, _, _ := fixture(t)
			w2.store = store
			w2.exec = f.run
			r.ReconcileOnly = true
			commit, err = w2.ResolveSource(context.Background(), r)
			if err != nil || commit != SourceCommit {
				t.Fatalf("reconcile: %q %v", commit, err)
			}
			if f.creates != 1 || f.starts != 1 || f.collects != 1 {
				t.Fatalf("reexecuted operation: %+v", f)
			}
			refs, err := w2.RunEvidence(context.Background(), r.RunID)
			if err != nil || len(refs) != 1 {
				t.Fatalf("retained source manifests %v %v", refs, err)
			}
			if err = w2.CleanupRun(context.Background(), r.RunID); err != nil || f.removes != 1 || f.exists {
				t.Fatalf("cleanup failed: %v", err)
			}
		})
	}
}

func TestReconcileMissingConflictAndTransportNeverCreate(t *testing.T) {
	for _, fault := range []string{"missing", "conflict", "transport"} {
		t.Run(fault, func(t *testing.T) {
			w, r, _ := fixture(t)
			f := &fakeEngine{t: t, inspectionFailure: fault == "transport"}
			if fault == "conflict" {
				f.exists = true
				f.state.ID = "foreign"
			}
			w.exec = f.run
			r.ReconcileOnly = true
			_, err := w.ResolveSource(context.Background(), r)
			var failure *testgenflow.WorkerFailure
			if !errors.As(err, &failure) || !failure.OutcomeUnknown || f.creates != 0 || f.starts != 0 {
				t.Fatalf("unsafe reconciliation: %v creates=%d starts=%d", err, f.creates, f.starts)
			}
		})
	}
}

func TestEvidenceReadbackRequiredBeforeSealOrDelete(t *testing.T) {
	w, r, s := fixture(t)
	f := &fakeEngine{t: t, files: sourceFiles()}
	w.exec = f.run
	s.failGet = true
	if _, err := w.ResolveSource(context.Background(), r); err == nil {
		t.Fatal("expected readback failure")
	}
	if !f.state.State.Running || f.receipt != "" || f.removes != 0 {
		t.Fatal("worker released evidence before verified retention")
	}
	f.state.State.Running = false
	f.state.State.Status = "exited"
	if err := w.CleanupRun(context.Background(), r.RunID); err == nil || f.removes != 0 {
		t.Fatal("deleted worker without verified receipt")
	}
}

func TestSealedEngineExitMustMatchEvidence(t *testing.T) {
	w, r, _ := fixture(t)
	f := &fakeEngine{t: t, files: sourceFiles()}
	w.exec = func(ctx context.Context, stdin []byte, limit int64, args ...string) ([]byte, error) {
		out, err := f.run(ctx, stdin, limit, args...)
		if args[len(args)-1] == "seal" {
			f.state.State.ExitCode = 143
		}
		return out, err
	}
	commit, err := w.ResolveSource(context.Background(), r)
	var failure *testgenflow.WorkerFailure
	if commit != "" || !errors.As(err, &failure) || !failure.OutcomeUnknown {
		t.Fatalf("promoted mismatched engine exit: %q %v", commit, err)
	}
}

func TestConfirmOperationsDetectsMissingAmongExtantWorkers(t *testing.T) {
	w, r, _ := fixture(t)
	f := &fakeEngine{t: t, files: sourceFiles()}
	w.exec = f.run
	if _, err := w.ResolveSource(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := w.ConfirmOperations(context.Background(), r.RunID, []string{r.IdempotencyKey}); err != nil {
		t.Fatal(err)
	}
	err := w.ConfirmOperations(context.Background(), r.RunID, []string{r.IdempotencyKey, r.IdempotencyKey + ":missing"})
	var failure *testgenflow.WorkerFailure
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || f.creates != 1 || f.removes != 0 {
		t.Fatalf("missing journaled worker not detected: %v", err)
	}
}

func TestCancellationCollectsAndConfirmsBeforeReturningDigest(t *testing.T) {
	w, r, _ := fixture(t)
	f := &fakeEngine{t: t, files: sourceFiles()}
	w.exec = f.run
	if _, err := w.ResolveSource(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	f.state.State.Running = true
	f.state.State.Status = "running"
	f.receipt = ""
	ref, err := w.CancelRun(context.Background(), r.RunID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := w.get(context.Background(), ref, MaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		TerminationConfirmed bool     `json:"termination_confirmed"`
		Refs                 []string `json:"evidence_refs"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.TerminationConfirmed || len(result.Refs) != 1 || f.state.State.Running {
		t.Fatalf("unconfirmed cancellation: %s", raw)
	}
	if err = w.CleanupRun(context.Background(), r.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = w.CancelRun(context.Background(), r.RunID); err == nil {
		t.Fatal("absence cannot prove cancellation")
	}
}

func TestCancellationRetentionFailureIsUnknownButStopsWorker(t *testing.T) {
	w, r, store := fixture(t)
	f := &fakeEngine{t: t, files: sourceFiles()}
	w.exec = f.run
	if _, err := w.ResolveSource(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	f.state.State.Running = true
	f.state.State.Status = "running"
	store.failGet = true
	ref, err := w.CancelRun(context.Background(), r.RunID)
	var failure *testgenflow.WorkerFailure
	if ref != "" || !errors.As(err, &failure) || !failure.OutcomeUnknown || f.state.State.Running || f.removes != 0 {
		t.Fatalf("unsafe cancellation after retention failure: %q %v %+v", ref, err, f.state)
	}
}

func TestPrecancelledRequestDoesNotCreate(t *testing.T) {
	w, r, _ := fixture(t)
	f := &fakeEngine{t: t}
	w.exec = f.run
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.ResolveSource(ctx, r); !errors.Is(err, context.Canceled) || len(f.calls) != 0 {
		t.Fatalf("cancelled request dispatched: %v %v", err, f.calls)
	}
}

func TestSandboxArgumentsContainBoundsAndNoMountsOrInheritedEnvironment(t *testing.T) {
	w, _, _ := fixture(t)
	args := w.sandboxArgs("name", "run", "digest", "bridge", 1800)
	for _, want := range []string{"--read-only", "--image-volume=ignore", "--unsetenv-all", "--http-proxy=false", "--user=65532:65532", "--memory=3g", "--memory-swap=3g", "--pids-limit=256", "--cpus=2", "--timeout=1800", "--security-opt=no-new-privileges", "--ulimit=fsize=536870912:536870912"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--volume") || strings.HasPrefix(arg, "--mount") || strings.HasPrefix(arg, "--env-file") || strings.Contains(arg, "socket") {
			t.Fatalf("unsafe sandbox argument %s", arg)
		}
	}
	unit, err := LoadProfile(unitProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if args := (&Worker{profile: unit}).sandboxArgs("name", "run", "digest", "bridge", 1800); !slices.Contains(args, "--env=ATG_TEST_NETWORK_MODE=offline") || !slices.Contains(args, "--env=ATG_SUREFIRE_PROVIDER="+unit.SurefireProvider) {
		t.Fatalf("offline mode or test provider was not bound into the worker environment: %v", args)
	}
}

func TestProfileAndArtifactValidation(t *testing.T) {
	w, r, s := fixture(t)
	if len(w.profile.ExpectedTestIDs()) != 125 {
		t.Fatal("baseline inventory changed")
	}
	raw := []byte(unitTestArtifact)
	ref, err := s.Put(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	r.Request.InputArtifactDigest = ref.Digest
	got, err := w.PrepareTestArtifact(context.Background(), r)
	if err != nil || len(got.ChangedTestPaths) != 1 || got.TestArtifactDigest != ref.Digest {
		t.Fatalf("admission %v %v", got, err)
	}
	r.TestArtifactDigest = got.TestArtifactDigest
	r.PatchDigest = got.PatchDigest
	r.ChangedTestPaths = got.ChangedTestPaths
	if _, err = w.admitted(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.PatchDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err = w.admitted(context.Background(), r); err == nil {
		t.Fatal("accepted forged patch binding")
	}
	for _, bad := range []string{
		strings.Replace(string(raw), "\"schema_version\": \"1\"", "\"schema_version\": \"1\", \"Schema_Version\": \"1\"", 1),
		strings.Replace(string(raw), "schema_version", "Schema_Version", 1),
		strings.Replace(string(raw), "src/test/java/", "src/test/java/../", 1),
		strings.Replace(string(raw), "ArithmeticSmokeTest.java", "pom.xml", 1),
		strings.Replace(string(raw), "ArithmeticSmokeTest#addsWithoutOverflow", "ForeignTest#addsWithoutOverflow", 1),
		string(raw) + "{}",
	} {
		if _, err := DecodeTestArtifact([]byte(bad)); err == nil {
			t.Errorf("accepted unsafe input: %s", bad)
		}
	}
	profileRaw := append([]byte(nil), w.profile.raw...)
	if _, err := DecodeProfile(bytes.Replace(profileRaw, []byte("public-egress-development"), []byte("network-allowlisted"), 1)); err == nil {
		t.Fatal("accepted unenforced network policy")
	}
}

func TestReadyBindsAndFreezesImageInRetainedProfile(t *testing.T) {
	p := unitProfile(t)
	store := &memoryStore{objects: map[string][]byte{}}
	image := strings.Repeat("a", 64)
	var targets []string
	run := func(_ context.Context, _ []byte, _ int64, args ...string) ([]byte, error) {
		switch args[0] {
		case "info":
			return []byte(`{"host":{"os":"linux","security":{"rootless":true}}}`), nil
		case "image":
			targets = append(targets, args[2])
			id := image
			if args[2] != DefaultImage {
				id = strings.TrimPrefix(args[2], "sha256:")
			}
			return json.Marshal([]any{map[string]any{"Id": id, "Config": map[string]any{"Labels": map[string]string{"io.lightpath.atg.worker": p.WorkerRecipe}}}})
		case "run":
			return []byte("java17"), nil
		}
		t.Fatalf("unexpected readiness command %v", args)
		return nil, nil
	}
	w, err := New(Config{Profile: p, Artifacts: store})
	if err != nil {
		t.Fatal(err)
	}
	w.exec = run
	if w.ProfileDigest() != "" {
		t.Fatal("unvalidated worker already has dispatch profile")
	}
	if err = w.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := w.ProfileDigest()
	if first == p.Digest || first == "" {
		t.Fatal("dispatch profile does not bind image")
	}
	raw, err := w.get(context.Background(), first, MaxEvidenceBytes)
	if err != nil || !bytes.Contains(raw, []byte(`"local_image_id":"sha256:`+image+`"`)) || !bytes.Contains(raw, []byte(p.Digest)) {
		t.Fatalf("missing retained binding: %s %v", raw, err)
	}
	image = strings.Repeat("b", 64)
	if err = w.Ready(context.Background()); err != nil || w.ProfileDigest() != first || targets[1] != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("tag change rebound live adapter: %s %v %v", w.ProfileDigest(), targets, err)
	}
	w2, err := New(Config{Profile: p, Artifacts: store})
	if err != nil {
		t.Fatal(err)
	}
	w2.exec = run
	if err = w2.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w2.ProfileDigest() == first {
		t.Fatal("new image claims previous profile identity")
	}
	_, r, _ := fixture(t)
	r.Request.ProfileDigest = first
	if err = w2.validateRequest(r); err == nil {
		t.Fatal("new image admitted an old profile request")
	}
}

func TestReadyDoesNotWaitForActiveOperation(t *testing.T) {
	w, r, _ := fixture(t)
	f := &fakeEngine{t: t, files: sourceFiles()}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	w.exec = func(ctx context.Context, stdin []byte, limit int64, args ...string) ([]byte, error) {
		switch args[0] {
		case "info":
			return []byte(`{"host":{"os":"linux","security":{"rootless":true}}}`), nil
		case "image":
			return json.Marshal([]any{map[string]any{"Id": strings.Repeat("a", 64), "Config": map[string]any{"Labels": map[string]string{"io.lightpath.atg.worker": w.profile.WorkerRecipe}}}})
		case "run":
			return nil, nil
		case "exec":
			if args[len(args)-1] == "status" {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		return f.run(ctx, stdin, limit, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := w.ResolveSource(ctx, r); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("operation did not start")
	}
	ready := make(chan error, 1)
	go func() { ready <- w.Ready(ctx) }()
	select {
	case err := <-ready:
		if err != nil {
			close(release)
			t.Fatal(err)
		}
	case <-ctx.Done():
		close(release)
		t.Fatal("readiness blocked behind workload")
	}
	// Another workload still respects cancellation while waiting for the slot.
	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondCancel()
	if _, err := w.ResolveSource(secondCtx, r); !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("queued request ignored cancellation: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMavenPhasesRequireExactIndependentInventory(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		phase                                testgenflow.MavenPhase
		missing, skipped, failed, unexpected bool
		exit                                 int
		want                                 bool
	}{
		{name: "baseline125", phase: testgenflow.MavenPhasePackage, want: true},
		{name: "baselineMissing", phase: testgenflow.MavenPhasePackage, missing: true},
		{name: "baselineSkipped", phase: testgenflow.MavenPhasePackage, skipped: true},
		{name: "baselineFailed", phase: testgenflow.MavenPhasePackage, failed: true},
		{name: "baselineExit", phase: testgenflow.MavenPhasePackage, exit: 1},
		{name: "focusedArtifact", phase: testgenflow.MavenPhaseFocusedTests, want: true},
		{name: "focusedStale", phase: testgenflow.MavenPhaseFocusedTests, unexpected: true},
		{name: "verify126", phase: testgenflow.MavenPhaseFinalVerify, want: true},
		{name: "verifyMissingArtifact", phase: testgenflow.MavenPhaseFinalVerify, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, r, store := fixture(t)
			expected := w.profile.ExpectedTestIDs()
			if tc.phase != testgenflow.MavenPhasePackage {
				raw := []byte(unitTestArtifact)
				ref, err := store.Put(context.Background(), raw)
				if err != nil {
					t.Fatal(err)
				}
				r.Request.InputArtifactDigest = ref.Digest
				generated, err := w.PrepareTestArtifact(context.Background(), r)
				if err != nil {
					t.Fatal(err)
				}
				r.TestArtifactDigest = generated.TestArtifactDigest
				r.PatchDigest = generated.PatchDigest
				r.ChangedTestPaths = generated.ChangedTestPaths
				a, err := DecodeTestArtifact(raw)
				if err != nil {
					t.Fatal(err)
				}
				if tc.phase == testgenflow.MavenPhaseFocusedTests {
					expected = a.ExpectedTestIDs
				} else {
					expected = append(expected, a.ExpectedTestIDs...)
				}
			}
			var xml strings.Builder
			xml.WriteString("<testsuite>")
			for i, id := range expected {
				if tc.missing && i == len(expected)-1 {
					continue
				}
				parts := strings.Split(id, "#")
				fmt.Fprintf(&xml, `<testcase classname="%s" name="%s">`, parts[0], parts[1])
				if tc.skipped && i == 0 {
					xml.WriteString("<skipped/>")
				}
				if tc.failed && i == 0 {
					xml.WriteString("<failure/>")
				}
				xml.WriteString("</testcase>")
			}
			if tc.unexpected {
				xml.WriteString(`<testcase classname="other.StaleTest" name="old"/>`)
			}
			xml.WriteString("</testsuite>")
			files := sourceFiles()
			files["exit"] = []byte(fmt.Sprintf("%d\n", tc.exit))
			files["reports/TEST-all.xml"] = []byte(xml.String())
			f := &fakeEngine{t: t, files: files}
			f.state.State.ExitCode = tc.exit
			w.exec = f.run
			// create initializes state, but an exit-1 protocol response is independent
			// of command success; the retained result must never pass it.
			if tc.exit != 0 {
				f.status = "1\n"
			}
			result, err := w.RunMaven(context.Background(), testgenflow.MavenWorkRequest{WorkRequest: r, Phase: tc.phase})
			if err != nil {
				t.Fatal(err)
			}
			if result.Succeeded != tc.want || result.EvidenceRef == "" {
				t.Fatalf("result %+v want %v", result, tc.want)
			}
			raw, err := w.get(context.Background(), result.EvidenceRef, MaxEvidenceBytes)
			if err != nil {
				t.Fatal(err)
			}
			var evidence Evidence
			if json.Unmarshal(raw, &evidence) != nil || len(evidence.ExpectedTestIDs) != len(expected) || evidence.Artifacts["reports/TEST-all.xml"] != artifact.Digest(files["reports/TEST-all.xml"]) || evidence.ProfileDigest != w.ProfileDigest() {
				t.Fatalf("lost report provenance: %s", raw)
			}
		})
	}
}

func TestReportInventoryNeverPassesMissingSkippedStaleOrDuplicate(t *testing.T) {
	expected := []string{"a.BTest#one", "a.BTest#two"}
	for _, tc := range []struct {
		name, xml string
		ok        bool
	}{
		{"all", `<testsuite><testcase classname="a.BTest" name="one"/><testcase classname="a.BTest" name="two"/></testsuite>`, true},
		{"missing", `<testsuite><testcase classname="a.BTest" name="one"/></testsuite>`, false},
		{"skipped", `<testsuite><testcase classname="a.BTest" name="one"><skipped/></testcase><testcase classname="a.BTest" name="two"><skipped/></testcase></testsuite>`, false},
		{"stale", `<testsuite><testcase classname="other.Test" name="old"/></testsuite>`, false},
		{"disabled", `<testsuite><testcase classname="a.BTest" name="one" status="notrun"/></testsuite>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := summarize(map[string][]byte{"reports/TEST-one.xml": []byte(tc.xml)}, expected)
			pass := err == nil && s.EvidenceComplete && len(s.ExecutedTestIDs) == 2 && len(s.FailedTestIDs) == 0 && len(s.SkippedTestIDs) == 0
			if pass != tc.ok {
				t.Fatalf("unexpected summary %+v %v", s, err)
			}
		})
	}
	if _, err := summarize(nil, expected); err == nil {
		t.Fatal("accepted missing reports")
	}
	xml := []byte(`<testsuite><testcase classname="a.BTest" name="one"/></testsuite>`)
	if _, err := summarize(map[string][]byte{"reports/TEST-one.xml": xml, "reports/TEST-two.xml": xml}, expected); err == nil {
		t.Fatal("accepted duplicated tests across files")
	}
}

func TestCollectionRejectsLinksTraversalAndUnknownFiles(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "reports/../exit", "settings.xml", "reports/not-xml"} {
		if _, err := decodeCollection(bundle(t, map[string][]byte{name: []byte("x")})); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "reports/TEST-a.xml", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.Close()
	if _, err := decodeCollection(b.Bytes()); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestOCIReadinessSourceRecoveryAndCleanup(t *testing.T) {
	if os.Getenv("ATG_TEST_LOCALWORKER_OCI") != "1" {
		t.Skip("set ATG_TEST_LOCALWORKER_OCI=1 for rootless Podman protocol check")
	}
	p, err := LoadProfile(unitProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.NewFileStore(t.TempDir(), MaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := New(Config{Profile: p, Artifacts: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = w.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	_, r, _ := fixture(t)
	r.Request.ProfileVersion = p.Version
	r.Request.ProfileDigest = w.ProfileDigest()
	r.RunID = fmt.Sprintf("oci-source-%d", time.Now().UnixNano())
	r.IdempotencyKey = r.RunID + ":source"
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = w.CancelRun(cleanupCtx, r.RunID)
		if err = w.CleanupRun(cleanupCtx, r.RunID); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}()
	commit, err := w.ResolveSource(ctx, r)
	if err != nil || commit != SourceCommit {
		t.Fatalf("source %q: %v", commit, err)
	}
	w2, err := New(Config{Profile: p, Artifacts: store})
	if err != nil {
		t.Fatal(err)
	}
	if err = w2.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	r.ReconcileOnly = true
	if commit, err = w2.ResolveSource(ctx, r); err != nil || commit != SourceCommit {
		t.Fatalf("restart source %q: %v", commit, err)
	}
	refs, err := w2.RunEvidence(ctx, r.RunID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("source evidence %v: %v", refs, err)
	}
	if err = w2.CleanupRun(ctx, r.RunID); err != nil {
		t.Fatal(err)
	}
}

func TestOCIAdmissionRejectsOverwriteAndCancellationRetainsEvidence(t *testing.T) {
	if os.Getenv("ATG_TEST_LOCALWORKER_OCI") != "1" {
		t.Skip("set ATG_TEST_LOCALWORKER_OCI=1 for rootless Podman admission/cancel check")
	}
	p, err := LoadProfile(unitProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.NewFileStore(t.TempDir(), MaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := New(Config{Profile: p, Artifacts: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = w.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	runID := fmt.Sprintf("oci-admission-%d", time.Now().UnixNano())
	input := artifact.Digest([]byte(runID))
	name := containerName(runID + ":focused")
	args := append([]string{"create"}, w.sandboxArgs(name, runLabel(runID), input, "bridge", 120)...)
	args = append(args, w.imageID, "run", "focused_tests", p.testSelector("package", TestArtifact{}), p.RepositoryURL, p.Commit)
	if _, err = w.exec(ctx, nil, 4096, args...); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = w.CancelRun(cleanupCtx, runID)
		if err := w.CleanupRun(cleanupCtx, runID); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}()
	if _, err = w.exec(ctx, nil, 4096, "start", name); err != nil {
		t.Fatal(err)
	}
	state, exists, err := w.inspect(ctx, name)
	if err != nil || !exists {
		t.Fatalf("inspect %v %v", exists, err)
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		status, err := w.exec(ctx, nil, 128, "exec", state.ID, "/bin/bash", "/opt/atg/worker.sh", "status")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(status)) == "input" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	original, err := w.exec(ctx, nil, 4096, "exec", state.ID, "cat", "/work/repo/src/test/java/com/redhat/lightwell/service/StatementImportServiceTest.java")
	if err != nil {
		t.Fatal(err)
	}
	bad := TestArtifact{Files: []TestFile{{Path: "src/test/java/com/redhat/lightwell/service/StatementImportServiceTest.java", Content: "overwrite\n"}}}
	if _, err = w.exec(ctx, bad.archive(), 4096, "exec", "-i", state.ID, "/bin/bash", "/opt/atg/worker.sh", "admit", artifact.Digest([]byte("bad"))); err == nil {
		t.Fatal("worker allowed original test overwrite")
	}
	after, err := w.exec(ctx, nil, 4096, "exec", state.ID, "cat", "/work/repo/src/test/java/com/redhat/lightwell/service/StatementImportServiceTest.java")
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("original source changed: %v", err)
	}
	ref, err := w.CancelRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.get(ctx, ref, MaxEvidenceBytes); err != nil {
		t.Fatal(err)
	}
	refs, err := w.RunEvidence(ctx, runID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("cancelled evidence %v %v", refs, err)
	}
	raw, err := w.get(ctx, refs[0], MaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	var evidence Evidence
	if json.Unmarshal(raw, &evidence) != nil || !evidence.Partial || evidence.Succeeded || len(evidence.Artifacts["command.log"]) == 0 {
		t.Fatalf("invalid partial evidence %s", raw)
	}
	if err = w.CleanupRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
}
