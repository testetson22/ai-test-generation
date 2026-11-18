package tekton

import (
	"bytes"
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

func TestProviderRejectsReplacementUIDBeforeSideEffects(t *testing.T) {
	for _, operation := range []string{"submit", "cancel", "cleanup"} {
		t.Run(operation, func(t *testing.T) {
			api := newFakeAPI()
			adapter := newTestProvider(t, validConfig(TaskRunKind, "atg-worker"), api, &fakeEvidence{durable: true})
			request := validRequest()
			job, err := adapter.Submit(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			request.KnownJob = job
			if operation == "cleanup" {
				api.complete(request)
			}
			replacement := api.resourceFor(request)
			replacement.Metadata.UID = "replacement-uid"
			api.save(replacement)
			switch operation {
			case "submit":
				_, err = adapter.Submit(context.Background(), request)
			case "cancel":
				_, err = adapter.Cancel(context.Background(), request)
			case "cleanup":
				err = adapter.Cleanup(context.Background(), request)
			}
			if !errors.Is(err, provider.ErrAttemptConflict) || api.createCalls != 1 || api.cancelCalls != 0 || api.deleteCalls != 0 || api.confirmCalls != 0 {
				t.Fatalf("replacement %s: err=%v creates=%d cancels=%d deletes=%d confirmations=%d", operation, err, api.createCalls, api.cancelCalls, api.deleteCalls, api.confirmCalls)
			}
			current := api.resourceFor(request)
			if current.Metadata.UID != replacement.Metadata.UID || current.Metadata.ResourceVersion != replacement.Metadata.ResourceVersion || !bytes.Equal(current.Spec, replacement.Spec) {
				t.Fatalf("replacement was mutated: before=%#v after=%#v", replacement, current)
			}
		})
	}
}

func TestProviderNotFoundPreservesKnownJobWithoutClaimingTermination(t *testing.T) {
	api := newFakeAPI()
	adapter := newTestProvider(t, validConfig(TaskRunKind, "atg-worker"), api, &fakeEvidence{durable: true})
	request := validRequest()
	request.KnownJob = provider.JobRef{ID: "atg-test/" + resourceName(TaskRunKind, request.SubmissionToken) + ":original-uid"}
	if _, err := adapter.Submit(context.Background(), request); err == nil || api.createCalls != 0 {
		t.Fatalf("known missing job was recreated: err=%v creates=%d", err, api.createCalls)
	}
	for _, observe := range []func(context.Context, provider.SubmitRequest) (provider.Observation, error){adapter.Observe, adapter.Cancel} {
		observation, err := observe(context.Background(), request)
		if err != nil || observation.Status != provider.StatusNotFound || observation.Job != request.KnownJob || observation.TerminationConfirmed {
			t.Fatalf("missing known job: observation=%#v err=%v", observation, err)
		}
		if err := observation.ValidateFor(request); err != nil {
			t.Fatalf("missing job observation lost reconciliation binding: %v", err)
		}
	}
	if api.cancelCalls != 0 || api.deleteCalls != 0 {
		t.Fatalf("missing job was mutated: cancels=%d deletes=%d", api.cancelCalls, api.deleteCalls)
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

func TestCleanupReconcilesOwnedWorkloadsAfterRunDeletionAndRestart(t *testing.T) {
	api := newFakeAPI()
	evidence := &fakeEvidence{durable: true}
	config := validConfig(TaskRunKind, "atg-worker")
	adapter := newTestProvider(t, config, api, evidence)
	request := validRequest()
	job, err := adapter.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.KnownJob = job
	api.complete(request)
	api.deleteRunWhilePending = true
	if err := adapter.Cleanup(context.Background(), request); !errors.Is(err, ErrCleanupPending) || api.deleteCalls != 1 || len(api.resources) != 0 || !api.ownedWorkloads[job.ID] {
		t.Fatalf("pending deletion: err=%v deletes=%d runs=%d workloads=%v", err, api.deleteCalls, len(api.resources), api.ownedWorkloads)
	}
	if err := adapter.Cleanup(context.Background(), request); !errors.Is(err, ErrCleanupPending) || api.confirmCalls != 1 || api.deleteCalls != 1 {
		t.Fatalf("run disappearance implied workload absence: err=%v confirmations=%d deletes=%d", err, api.confirmCalls, api.deleteCalls)
	}

	// Only the fake server's resources survive; neither reconstructed client nor
	// provider has a process-local record that deletion was requested.
	restartedAPI := newFakeAPI()
	restartedAPI.resources, restartedAPI.ownedWorkloads = api.resources, api.ownedWorkloads
	adapter = newTestProvider(t, config, restartedAPI, evidence)
	if err := adapter.Cleanup(context.Background(), request); !errors.Is(err, ErrCleanupPending) || restartedAPI.confirmCalls != 1 || restartedAPI.deleteCalls != 0 {
		t.Fatalf("restart forgot pending owned workloads: err=%v confirmations=%d deletes=%d", err, restartedAPI.confirmCalls, restartedAPI.deleteCalls)
	}
	if jobReference(restartedAPI.confirmedResource) != job || restartedAPI.confirmedResource.Kind != config.Kind || restartedAPI.confirmedResource.Metadata.ResourceVersion != "" {
		t.Fatalf("confirmation did not use persisted job identity: %#v", restartedAPI.confirmedResource)
	}
	delete(restartedAPI.ownedWorkloads, job.ID)
	if err := adapter.Cleanup(context.Background(), request); err != nil || restartedAPI.confirmCalls != 2 || restartedAPI.deleteCalls != 0 {
		t.Fatalf("workload deletion was not confirmed: err=%v confirmations=%d deletes=%d", err, restartedAPI.confirmCalls, restartedAPI.deleteCalls)
	}
	if err := adapter.Cleanup(context.Background(), request); err != nil {
		t.Fatalf("confirmed deletion is not idempotent: %v", err)
	}
}

func TestCleanupMissingRunWithoutKnownUIDCannotConfirmDeletion(t *testing.T) {
	api := newFakeAPI()
	adapter := newTestProvider(t, validConfig(TaskRunKind, "atg-worker"), api, &fakeEvidence{durable: true})
	request := validRequest()
	if err := adapter.Cleanup(context.Background(), request); !errors.Is(err, ErrCleanupNotReady) || api.confirmCalls != 0 || api.deleteCalls != 0 {
		t.Fatalf("unknown job absence accepted: err=%v confirmations=%d deletes=%d", err, api.confirmCalls, api.deleteCalls)
	}
	request.KnownJob = provider.JobRef{ID: "atg-test/another-run:uid-1"}
	if err := adapter.Cleanup(context.Background(), request); !errors.Is(err, provider.ErrAttemptConflict) || api.confirmCalls != 0 {
		t.Fatalf("unrelated persisted job accepted: err=%v confirmations=%d", err, api.confirmCalls)
	}
}

func TestProviderAcceptsNestedKeyReorderingButRejectsChangedValue(t *testing.T) {
	api := newFakeAPI()
	adapter := newTestProvider(t, validConfig(TaskRunKind, "atg-worker"), api, &fakeEvidence{durable: true})
	request := validRequest()
	job, err := adapter.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.KnownJob = job
	resource := api.resourceFor(request)
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(resource.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	spec["workspaces"] = json.RawMessage(`[{"emptyDir":{"sizeLimit":"8Gi"},"name":"repository"}]`)
	spec["params"] = bytes.Replace(spec["params"], []byte(`{"name":"task_id","value":"task-1"}`), []byte(`{"value":"task-1","name":"task_id"}`), 1)
	resource.Spec, err = json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	api.save(resource)
	if observed, err := adapter.Observe(context.Background(), request); err != nil || observed.Job != job || observed.Status != provider.StatusPending {
		t.Fatalf("equivalent nested key order rejected: observation=%#v err=%v", observed, err)
	}
	if repeated, err := adapter.Submit(context.Background(), request); err != nil || repeated != job || api.createCalls != 1 {
		t.Fatalf("known-job reconciliation changed identity: job=%#v err=%v creates=%d", repeated, err, api.createCalls)
	}
	request.KnownJob = provider.JobRef{}
	if repeated, err := adapter.Submit(context.Background(), request); err != nil || repeated != job || api.createCalls != 2 || len(api.resources) != 1 {
		t.Fatalf("ambiguous-submission reconciliation rejected equivalent spec: job=%#v err=%v creates=%d resources=%d", repeated, err, api.createCalls, len(api.resources))
	}
	resource.Spec = bytes.Replace(resource.Spec, []byte(`"sizeLimit":"8Gi"`), []byte(`"sizeLimit":"9Gi"`), 1)
	api.save(resource)
	if _, err := adapter.Observe(context.Background(), request); !errors.Is(err, provider.ErrAttemptConflict) {
		t.Fatalf("changed nested spec value accepted: %v", err)
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
	resources             map[string]Resource
	ownedWorkloads        map[string]bool
	createCalls           int
	cancelCalls           int
	deleteCalls           int
	confirmCalls          int
	confirmedResource     Resource
	loseCreateAck         bool
	cleanupConfirmed      bool
	deleteRunWhilePending bool
	getErr                error
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{resources: make(map[string]Resource), ownedWorkloads: make(map[string]bool)}
}

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
	a.ownedWorkloads[jobReference(resource).ID] = true
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
		return a.ConfirmDeletion(context.Background(), resource)
	}
	if current.Metadata.UID != resource.Metadata.UID || current.Metadata.ResourceVersion != resource.Metadata.ResourceVersion {
		return false, provider.ErrAttemptConflict
	}
	if a.deleteRunWhilePending {
		delete(a.resources, key)
	}
	if !a.cleanupConfirmed {
		return false, nil
	}
	delete(a.resources, key)
	delete(a.ownedWorkloads, jobReference(resource).ID)
	return true, nil
}

func (a *fakeAPI) ConfirmDeletion(_ context.Context, resource Resource) (bool, error) {
	a.confirmCalls++
	a.confirmedResource = resource
	if resource.Metadata.UID == "" {
		return false, provider.ErrAttemptConflict
	}
	if current, exists := a.resources[resourceKey(resource.Metadata.Namespace, resource.Kind, resource.Metadata.Name)]; exists {
		if current.Metadata.UID != resource.Metadata.UID {
			return false, provider.ErrAttemptConflict
		}
		return false, nil
	}
	return !a.ownedWorkloads[jobReference(resource).ID], nil
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
