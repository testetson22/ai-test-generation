package tekton

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

func TestProviderReconcilesCreateAckLossAndRejectsTokenReuse(t *testing.T) {
	api := newFakeAPI()
	api.loseCreateAck = true
	providerAdapter := newTestProvider(t, validConfig(TaskRunKind, "atg-worker"), api, &fakeEvidence{durable: true})
	request := validRequest()

	if _, err := providerAdapter.Submit(context.Background(), request); err == nil {
		t.Fatal("lost create acknowledgement did not report uncertainty")
	} else {
		var submissionError *provider.SubmissionError
		if !errors.As(err, &submissionError) || !submissionError.OutcomeUnknown {
			t.Fatalf("create error = %v, want unknown submission", err)
		}
	}
	observation, err := providerAdapter.Observe(context.Background(), request)
	if err != nil || observation.Status != provider.StatusPending || observation.Job.ID == "" {
		t.Fatalf("reconciled observation = %#v, err %v", observation, err)
	}
	job, err := providerAdapter.Submit(context.Background(), request)
	if err != nil || job != observation.Job || api.createCalls != 2 || len(api.resources) != 1 {
		t.Fatalf("idempotent submit job=%#v err=%v creates=%d resources=%d", job, err, api.createCalls, len(api.resources))
	}

	conflicting := request
	conflicting.TaskSpecDigest = testDigest("d")
	if _, err := providerAdapter.Submit(context.Background(), conflicting); !errors.Is(err, provider.ErrAttemptConflict) {
		t.Fatalf("token reused with different task spec error = %v", err)
	}
}

func TestProviderSeparatesCancellationRequestFromTermination(t *testing.T) {
	api := newFakeAPI()
	providerAdapter := newTestProvider(t, validConfig(TaskRunKind, "atg-worker"), api, &fakeEvidence{durable: true})
	request := validRequest()
	if _, err := providerAdapter.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	requested, err := providerAdapter.Cancel(context.Background(), request)
	if err != nil || requested.Status != provider.StatusCancelRequested || requested.TerminationConfirmed {
		t.Fatalf("cancel request = %#v, err %v; want unconfirmed cancellation", requested, err)
	}
	api.confirmCancellation(request)
	confirmed, err := providerAdapter.Observe(context.Background(), request)
	if err != nil || confirmed.Status != provider.StatusCancelled || !confirmed.TerminationConfirmed {
		t.Fatalf("cancellation observation = %#v, err %v", confirmed, err)
	}
}

func TestProviderPropagatesStatusApiInterruptionWithoutInventingOutcome(t *testing.T) {
	api := newFakeAPI()
	providerAdapter := newTestProvider(t, validConfig(TaskRunKind, "atg-worker"), api, &fakeEvidence{durable: true})
	request := validRequest()
	if _, err := providerAdapter.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	api.getErr = errors.New("status API interrupted")
	if observation, err := providerAdapter.Observe(context.Background(), request); err == nil || observation.Status != "" {
		t.Fatalf("interrupted API yielded observation %#v, err %v", observation, err)
	}
}

func TestProviderJobIdentityIncludesResourceUID(t *testing.T) {
	api := newFakeAPI()
	providerAdapter := newTestProvider(t, validConfig(TaskRunKind, "atg-worker"), api, &fakeEvidence{durable: true})
	request := validRequest()
	first, err := providerAdapter.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.resourceFor(request)
	resource.Metadata.UID = "replacement-uid"
	api.save(resource)
	observation, err := providerAdapter.Observe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Job.ID == first.ID || !strings.Contains(observation.Job.ID, "replacement-uid") {
		t.Fatalf("provider did not preserve changed UID identity: submitted=%q observed=%q", first.ID, observation.Job.ID)
	}
}

func TestProviderWillNotPruneBeforeEvidenceAndConfirmedTermination(t *testing.T) {
	api := newFakeAPI()
	evidence := &fakeEvidence{}
	providerAdapter := newTestProvider(t, validConfig(TaskRunKind, "atg-worker"), api, evidence)
	request := validRequest()
	if _, err := providerAdapter.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := providerAdapter.Cleanup(context.Background(), request); !errors.Is(err, ErrEvidenceNotDurable) || api.deleteCalls != 0 {
		t.Fatalf("cleanup before evidence acknowledgement = %v, deletes %d", err, api.deleteCalls)
	}
	evidence.durable = true
	if err := providerAdapter.Cleanup(context.Background(), request); !errors.Is(err, ErrCleanupNotReady) || api.deleteCalls != 0 {
		t.Fatalf("cleanup before termination = %v, deletes %d", err, api.deleteCalls)
	}
	api.complete(request)
	if err := providerAdapter.Cleanup(context.Background(), request); !errors.Is(err, ErrCleanupPending) || api.deleteCalls != 1 {
		t.Fatalf("unconfirmed cleanup = %v, deletes %d", err, api.deleteCalls)
	}
	api.cleanupConfirmed = true
	if err := providerAdapter.Cleanup(context.Background(), request); err != nil {
		t.Fatalf("confirmed cleanup: %v", err)
	}
	if api.deleteCalls != 2 || len(api.resources) != 0 {
		t.Fatalf("confirmed cleanup state: deletes=%d resources=%d", api.deleteCalls, len(api.resources))
	}
}

func TestObservationFixturesPreservePendingFailureCompletionAndCancellationSemantics(t *testing.T) {
	tests := []struct {
		file        string
		kind        RunKind
		definition  string
		status      provider.Status
		failure     string
		termination bool
	}{
		{file: "taskrun-pending.status.json", kind: TaskRunKind, definition: "atg-worker", status: provider.StatusPending},
		{file: "taskrun-unschedulable.status.json", kind: TaskRunKind, definition: "atg-worker", status: provider.StatusPending, failure: "worker_resource_exhausted"},
		{file: "pipelinerun-running.status.json", kind: PipelineRunKind, definition: "atg-worker-pipeline", status: provider.StatusRunning},
		{file: "taskrun-completed.status.json", kind: TaskRunKind, definition: "atg-worker", status: provider.StatusCompleted, termination: true},
		{file: "taskrun-failed.status.json", kind: TaskRunKind, definition: "atg-worker", status: provider.StatusFailed, failure: "unknown", termination: true},
		{file: "taskrun-cancel-requested.status.json", kind: TaskRunKind, definition: "atg-worker", status: provider.StatusCancelRequested},
		{file: "taskrun-cancelled.status.json", kind: TaskRunKind, definition: "atg-worker", status: provider.StatusCancelled, termination: true},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			request := validRequest()
			config := validConfig(test.kind, test.definition)
			expected, err := Render(config, request)
			if err != nil {
				t.Fatal(err)
			}
			fixtureBytes, err := os.ReadFile(filepath.Join("testdata", test.file))
			if err != nil {
				t.Fatal(err)
			}
			var fixture Resource
			if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
				t.Fatal(err)
			}
			actual := expected
			actual.Metadata.UID = fixture.Metadata.UID
			actual.Metadata.ResourceVersion = fixture.Metadata.ResourceVersion
			actual.Status = fixture.Status
			if len(fixture.Spec) > 0 {
				var expectedSpec, fixtureSpec map[string]json.RawMessage
				if err := json.Unmarshal(expected.Spec, &expectedSpec); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(fixture.Spec, &fixtureSpec); err != nil {
					t.Fatal(err)
				}
				if status := fixtureSpec["status"]; len(status) > 0 {
					expectedSpec["status"] = status
				}
				actual.Spec, err = json.Marshal(expectedSpec)
				if err != nil {
					t.Fatal(err)
				}
				expected.Spec = actual.Spec
			}
			observation, err := ObservationFromResource(expected, request, actual, time.Date(2026, time.November, 18, 0, 1, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("normalize fixture: %v", err)
			}
			if observation.Status != test.status || observation.FailureCategory != test.failure || observation.TerminationConfirmed != test.termination {
				t.Fatalf("observation = %#v, want status=%s failure=%s terminated=%t", observation, test.status, test.failure, test.termination)
			}
			if observation.Sequence != 0 || observation.ProviderRevision != fixture.Metadata.ResourceVersion || !strings.Contains(observation.Job.ID, fixture.Metadata.UID) {
				t.Fatalf("observation lost fence or provider identity: %#v", observation)
			}
		})
	}
}

func TestObservationRejectsSubmissionAndRunSpecDrift(t *testing.T) {
	request := validRequest()
	expected, err := Render(validConfig(TaskRunKind, "atg-worker"), request)
	if err != nil {
		t.Fatal(err)
	}
	actual := expected
	actual.Metadata.UID = "uid-1"
	actual.Metadata.ResourceVersion = "opaque-rv-1"
	actual.Spec = json.RawMessage(`{"taskRef":{"name":"other-worker"}}`)
	if _, err := ObservationFromResource(expected, request, actual, time.Now().UTC()); !errors.Is(err, provider.ErrAttemptConflict) {
		t.Fatalf("changed task reference error = %v", err)
	}
	actual = expected
	actual.Metadata.UID = "uid-1"
	actual.Metadata.ResourceVersion = "opaque-rv-1"
	actual.Metadata.Annotations = cloneStringMap(expected.Metadata.Annotations)
	actual.Metadata.Annotations["atg-task-spec-digest"] = testDigest("e")
	if _, err := ObservationFromResource(expected, request, actual, time.Now().UTC()); !errors.Is(err, provider.ErrAttemptConflict) {
		t.Fatalf("changed task identity error = %v", err)
	}
}

func newTestProvider(t *testing.T, config Config, api *fakeAPI, evidence EvidenceGate) *Provider {
	t.Helper()
	providerAdapter, err := NewProvider(config, api, evidence, fixedTime{time.Date(2026, time.November, 18, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return providerAdapter
}

type fixedTime struct{ now time.Time }

func (c fixedTime) Now() time.Time { return c.now }

type fakeEvidence struct{ durable bool }

func (e *fakeEvidence) Durable(context.Context, provider.SubmitRequest) (bool, error) {
	return e.durable, nil
}

type fakeAPI struct {
	resources        map[string]Resource
	createCalls      int
	cancelCalls      int
	deleteCalls      int
	loseCreateAck    bool
	cleanupConfirmed bool
	getErr           error
}

func newFakeAPI() *fakeAPI { return &fakeAPI{resources: make(map[string]Resource)} }

func (a *fakeAPI) CreateRun(_ context.Context, resource Resource) (Resource, error) {
	a.createCalls++
	key := resourceKey(resource.Metadata.Namespace, resource.Kind, resource.Metadata.Name)
	if _, exists := a.resources[key]; exists {
		return Resource{}, ErrAlreadyExists
	}
	resource.Metadata.UID = "uid-" + resource.Metadata.Name
	resource.Metadata.ResourceVersion = "opaque-rv-1"
	resource.Status = &ResourceStatus{Conditions: []Condition{{Type: "Succeeded", Status: "Unknown", Reason: "TaskRunPending"}}}
	a.resources[key] = resource
	if a.loseCreateAck {
		a.loseCreateAck = false
		return Resource{}, ErrCreateAckLost
	}
	return resource, nil
}

func (a *fakeAPI) GetRun(_ context.Context, namespace string, kind RunKind, name string) (Resource, error) {
	if a.getErr != nil {
		return Resource{}, a.getErr
	}
	resource, exists := a.resources[resourceKey(namespace, kind, name)]
	if !exists {
		return Resource{}, ErrNotFound
	}
	return cloneResource(resource)
}

func (a *fakeAPI) RequestCancellation(_ context.Context, resource Resource) (Resource, error) {
	a.cancelCalls++
	key := resourceKey(resource.Metadata.Namespace, resource.Kind, resource.Metadata.Name)
	current, exists := a.resources[key]
	if !exists {
		return Resource{}, ErrNotFound
	}
	if current.Metadata.UID != resource.Metadata.UID || current.Metadata.ResourceVersion != resource.Metadata.ResourceVersion {
		return Resource{}, provider.ErrAttemptConflict
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(current.Spec, &spec); err != nil {
		return Resource{}, err
	}
	spec["status"] = json.RawMessage(`"Cancelled"`)
	current.Spec, _ = json.Marshal(spec)
	current.Metadata.ResourceVersion = "opaque-rv-cancel-requested"
	current.Status = &ResourceStatus{StartTime: timePointer(time.Date(2026, time.November, 18, 0, 0, 1, 0, time.UTC)), Conditions: []Condition{{Type: "Succeeded", Status: "Unknown", Reason: "TaskRunCancelled"}}}
	a.resources[key] = current
	return cloneResource(current)
}

func (a *fakeAPI) DeleteRun(_ context.Context, resource Resource) (bool, error) {
	a.deleteCalls++
	key := resourceKey(resource.Metadata.Namespace, resource.Kind, resource.Metadata.Name)
	current, exists := a.resources[key]
	if !exists {
		return true, nil
	}
	if current.Metadata.UID != resource.Metadata.UID {
		return false, provider.ErrAttemptConflict
	}
	if !a.cleanupConfirmed {
		return false, nil
	}
	delete(a.resources, key)
	return true, nil
}

func (a *fakeAPI) confirmCancellation(request provider.SubmitRequest) {
	resource := a.resourceFor(request)
	resource.Metadata.ResourceVersion = "opaque-rv-cancelled"
	resource.Status = &ResourceStatus{
		StartTime:      timePointer(time.Date(2026, time.November, 18, 0, 0, 1, 0, time.UTC)),
		CompletionTime: timePointer(time.Date(2026, time.November, 18, 0, 0, 5, 0, time.UTC)),
		Conditions:     []Condition{{Type: "Succeeded", Status: "False", Reason: "TaskRunCancelled"}},
	}
	a.save(resource)
}

func (a *fakeAPI) complete(request provider.SubmitRequest) {
	resource := a.resourceFor(request)
	resource.Metadata.ResourceVersion = "opaque-rv-completed"
	resource.Status = &ResourceStatus{
		StartTime:      timePointer(time.Date(2026, time.November, 18, 0, 0, 1, 0, time.UTC)),
		CompletionTime: timePointer(time.Date(2026, time.November, 18, 0, 0, 10, 0, time.UTC)),
		Conditions:     []Condition{{Type: "Succeeded", Status: "True", Reason: "Succeeded"}},
	}
	a.save(resource)
}

func (a *fakeAPI) resourceFor(request provider.SubmitRequest) Resource {
	resource, exists := a.resources[resourceKey("atg-test", TaskRunKind, resourceName(TaskRunKind, request.SubmissionToken))]
	if !exists {
		panic("expected fake Tekton resource")
	}
	return resource
}

func (a *fakeAPI) save(resource Resource) {
	a.resources[resourceKey(resource.Metadata.Namespace, resource.Kind, resource.Metadata.Name)] = resource
}

func resourceKey(namespace string, kind RunKind, name string) string {
	return namespace + "/" + string(kind) + "/" + name
}

func cloneResource(resource Resource) (Resource, error) {
	data, err := json.Marshal(resource)
	if err != nil {
		return Resource{}, err
	}
	var copy Resource
	if err := json.Unmarshal(data, &copy); err != nil {
		return Resource{}, err
	}
	return copy, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func timePointer(value time.Time) *time.Time { return &value }
