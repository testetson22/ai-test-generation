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
const unitTestArtifact = `{
  "schema_version": "1",
  "files": [{"path":"src/test/java/com/redhat/lightwell/atg/ArithmeticSmokeTest.java","content":"package com.redhat.lightwell.atg;\nclass ArithmeticSmokeTest {}\n"}],
  "expected_test_ids": ["com.redhat.lightwell.atg.ArithmeticSmokeTest#addsWithoutOverflow"]
}`

// Unit tests use synthetic inventories; only explicit OCI integration tests read
// the independently versioned workload profile outside this module.
func unitProfile(t *testing.T) Profile {
	t.Helper()
	methods := make([]string, 121)
	for i := range methods {
		methods[i] = fmt.Sprintf("behavior%d", i)
	}
	p := Profile{
		SchemaVersion: "1", ID: "ssc-java17-maven", Version: "1.0.0", RepositoryURL: SourceURL,
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
}

func TestProfileAndArtifactValidation(t *testing.T) {
	w, r, s := fixture(t)
	if len(w.profile.ExpectedTestIDs()) != 121 {
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
			return json.Marshal([]any{map[string]any{"Id": id, "Config": map[string]any{"Labels": map[string]string{"io.lightpath.atg.worker": workerLabel}}}})
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
			return json.Marshal([]any{map[string]any{"Id": strings.Repeat("a", 64), "Config": map[string]any{"Labels": map[string]string{"io.lightpath.atg.worker": workerLabel}}}})
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
		{name: "baseline121", phase: testgenflow.MavenPhasePackage, want: true},
		{name: "baselineMissing", phase: testgenflow.MavenPhasePackage, missing: true},
		{name: "baselineSkipped", phase: testgenflow.MavenPhasePackage, skipped: true},
		{name: "baselineFailed", phase: testgenflow.MavenPhasePackage, failed: true},
		{name: "baselineExit", phase: testgenflow.MavenPhasePackage, exit: 1},
		{name: "focusedArtifact", phase: testgenflow.MavenPhaseFocusedTests, want: true},
		{name: "focusedStale", phase: testgenflow.MavenPhaseFocusedTests, unexpected: true},
		{name: "verify122", phase: testgenflow.MavenPhaseFinalVerify, want: true},
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = w.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	_, r, _ := fixture(t)
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = w.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	runID := fmt.Sprintf("oci-admission-%d", time.Now().UnixNano())
	input := artifact.Digest([]byte(runID))
	name := containerName(runID + ":focused")
	args := append([]string{"create"}, w.sandboxArgs(name, runLabel(runID), input, "bridge", 120)...)
	args = append(args, w.imageID, "run", "focused_tests", "com.redhat.lightwell.ApplicationTest", p.RepositoryURL, p.Commit)
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
	original, err := w.exec(ctx, nil, 4096, "exec", state.ID, "cat", "/work/repo/src/test/java/com/redhat/lightwell/ApplicationTest.java")
	if err != nil {
		t.Fatal(err)
	}
	bad := TestArtifact{Files: []TestFile{{Path: "src/test/java/com/redhat/lightwell/ApplicationTest.java", Content: "overwrite\n"}}}
	if _, err = w.exec(ctx, bad.archive(), 4096, "exec", "-i", state.ID, "/bin/bash", "/opt/atg/worker.sh", "admit", artifact.Digest([]byte("bad"))); err == nil {
		t.Fatal("worker allowed original test overwrite")
	}
	after, err := w.exec(ctx, nil, 4096, "exec", state.ID, "cat", "/work/repo/src/test/java/com/redhat/lightwell/ApplicationTest.java")
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
