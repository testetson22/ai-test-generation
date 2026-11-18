package tekton

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

func TestRenderTaskRunAndPipelineRunAgainstPinnedV1Fixtures(t *testing.T) {
	tests := []struct {
		name string
		kind RunKind
		ref  string
		file string
	}{
		{name: "taskrun", kind: TaskRunKind, ref: "atg-worker", file: "taskrun-v1.spec.json"},
		{name: "pipelinerun", kind: PipelineRunKind, ref: "atg-worker-pipeline", file: "pipelinerun-v1.spec.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig(test.kind, test.ref)
			resource, err := Render(config, validRequest())
			if err != nil {
				t.Fatalf("render resource: %v", err)
			}
			fixture, err := os.ReadFile(filepath.Join("testdata", test.file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var expectedSpec, actualSpec any
			if err := json.Unmarshal(fixture, &expectedSpec); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if err := json.Unmarshal(resource.Spec, &actualSpec); err != nil {
				t.Fatalf("decode rendered spec: %v", err)
			}
			if !reflect.DeepEqual(actualSpec, expectedSpec) {
				t.Fatalf("rendered spec = %#v, want fixture %#v", actualSpec, expectedSpec)
			}
			if resource.APIVersion != APIVersion || resource.Kind != test.kind || resource.Metadata.Namespace != config.Namespace {
				t.Fatalf("resource identity = %#v", resource)
			}
			if resource.Metadata.Annotations["atg-task-id"] != "task-1" || resource.Metadata.Annotations["atg-attempt-id"] != "attempt-1" || resource.Metadata.Annotations["atg-task-spec-digest"] != validRequest().TaskSpecDigest {
				t.Fatalf("submission identity annotations = %#v", resource.Metadata.Annotations)
			}
			if strings.Contains(resource.Metadata.Name, validRequest().SubmissionToken) {
				t.Fatalf("resource name exposed raw submission token %q", resource.Metadata.Name)
			}
			first, err := Marshal(resource)
			if err != nil {
				t.Fatal(err)
			}
			repeated, err := Render(config, validRequest())
			if err != nil {
				t.Fatal(err)
			}
			second, err := Marshal(repeated)
			if err != nil || !bytes.Equal(first, second) {
				t.Fatalf("render is not deterministic: err=%v first=%s second=%s", err, first, second)
			}
		})
	}
}

func TestRenderRejectsUnsupportedOrUnsafeProviderConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config, *provider.SubmitRequest)
	}{
		{name: "native retry", mutate: func(config *Config, _ *provider.SubmitRequest) { config.NativeRetries = 1 }},
		{name: "unbounded workspace", mutate: func(config *Config, _ *provider.SubmitRequest) { config.WorkspaceSizeLimit = "" }},
		{name: "unpinned worker definition", mutate: func(config *Config, _ *provider.SubmitRequest) { config.DefinitionDigest = "" }},
		{name: "invalid timeout", mutate: func(config *Config, _ *provider.SubmitRequest) { config.TimeoutSeconds = 0 }},
		{name: "unvalidated reference name", mutate: func(config *Config, _ *provider.SubmitRequest) { config.DefinitionName = "../worker" }},
		{name: "invalid task digest", mutate: func(_ *Config, request *provider.SubmitRequest) { request.TaskSpecDigest = "sha256:short" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, request := validConfig(TaskRunKind, "atg-worker"), validRequest()
			test.mutate(&config, &request)
			if _, err := Render(config, request); err == nil {
				t.Fatal("invalid configuration or identity was accepted")
			}
		})
	}
}

func TestRenderBindsDifferentAttemptsToDifferentResources(t *testing.T) {
	config := validConfig(TaskRunKind, "atg-worker")
	first, err := Render(config, validRequest())
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest()
	request.SubmissionToken = "submission-attempt-2"
	request.AttemptID = "attempt-2"
	second, err := Render(config, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Metadata.Name == second.Metadata.Name || first.Metadata.Annotations["atg-attempt-id"] == second.Metadata.Annotations["atg-attempt-id"] {
		t.Fatalf("distinct attempt identity reused resource: first %#v second %#v", first.Metadata, second.Metadata)
	}
}

func validConfig(kind RunKind, definition string) Config {
	return Config{
		Kind: kind, Namespace: "atg-test", DefinitionName: definition,
		DefinitionDigest:   testDigest("d"),
		ServiceAccountName: "atg-worker", WorkspaceName: "repository",
		WorkspaceSizeLimit: "8Gi", TimeoutSeconds: 2700,
	}
}

func validRequest() provider.SubmitRequest {
	return provider.SubmitRequest{
		SubmissionToken: "submission-attempt-1", TaskID: "task-1", AttemptID: "attempt-1",
		TaskSpecDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProfileID:      "spring-profile", ProfileVersion: "1.0.0",
		ProfileDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
}

func testDigest(char string) string { return "sha256:" + strings.Repeat(char, 64) }
