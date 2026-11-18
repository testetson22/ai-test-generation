package tekton

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

const APIVersion = "tekton.dev/v1"

type RunKind string

const (
	TaskRunKind     RunKind = "TaskRun"
	PipelineRunKind RunKind = "PipelineRun"
)

var (
	ErrInvalidConfig = errors.New("invalid provisional Tekton renderer configuration")
	dnsLabelPattern  = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	quantityPattern  = regexp.MustCompile(`^[1-9][0-9]*(?:Ki|Mi|Gi|Ti)$`)
	digestPattern    = regexp.MustCompile(`^sha256:[A-Fa-f0-9]{64}$`)
)

type Config struct {
	Kind               RunKind
	Namespace          string
	DefinitionName     string
	DefinitionDigest   string
	ServiceAccountName string
	WorkspaceName      string
	WorkspaceSizeLimit string
	TimeoutSeconds     int64
	NativeRetries      int
}

type Resource struct {
	APIVersion string          `json:"apiVersion"`
	Kind       RunKind         `json:"kind"`
	Metadata   ObjectMeta      `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
	Status     *ResourceStatus `json:"status,omitempty"`
}

type ObjectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
}

type ResourceStatus struct {
	Conditions     []Condition `json:"conditions,omitempty"`
	StartTime      *time.Time  `json:"startTime,omitempty"`
	CompletionTime *time.Time  `json:"completionTime,omitempty"`
}

type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime,omitempty"`
}

type reference struct {
	Name string `json:"name"`
}

type parameter struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type emptyDir struct {
	SizeLimit string `json:"sizeLimit"`
}

type workspace struct {
	Name     string   `json:"name"`
	EmptyDir emptyDir `json:"emptyDir"`
}

type taskRunSpec struct {
	TaskRef            reference   `json:"taskRef"`
	Params             []parameter `json:"params"`
	ServiceAccountName string      `json:"serviceAccountName"`
	Timeout            string      `json:"timeout"`
	Workspaces         []workspace `json:"workspaces"`
}

type pipelineRunSpec struct {
	PipelineRef     reference   `json:"pipelineRef"`
	Params          []parameter `json:"params"`
	TaskRunTemplate struct {
		ServiceAccountName string `json:"serviceAccountName"`
	} `json:"taskRunTemplate"`
	Timeouts struct {
		Pipeline string `json:"pipeline"`
	} `json:"timeouts"`
	Workspaces []workspace `json:"workspaces"`
}

// Render creates a deterministic offline Tekton resource fixture. It does not
// submit or authorize a workload and is not a production provider adapter.
func Render(config Config, request provider.SubmitRequest) (Resource, error) {
	if err := validateConfig(config); err != nil {
		return Resource{}, err
	}
	if err := request.Validate(); err != nil {
		return Resource{}, fmt.Errorf("submission: %w", err)
	}
	params := []parameter{
		{Name: "task_id", Value: request.TaskID},
		{Name: "attempt_id", Value: request.AttemptID},
		{Name: "submission_token_sha256", Value: digest(request.SubmissionToken)},
		{Name: "task_spec_digest", Value: request.TaskSpecDigest},
		{Name: "profile_id", Value: request.ProfileID},
		{Name: "profile_version", Value: request.ProfileVersion},
		{Name: "profile_digest", Value: request.ProfileDigest},
	}
	workspaces := []workspace{{Name: config.WorkspaceName, EmptyDir: emptyDir{SizeLimit: config.WorkspaceSizeLimit}}}
	timeout := (time.Duration(config.TimeoutSeconds) * time.Second).String()
	var spec any
	switch config.Kind {
	case TaskRunKind:
		spec = taskRunSpec{
			TaskRef: reference{Name: config.DefinitionName}, Params: params,
			ServiceAccountName: config.ServiceAccountName, Timeout: timeout, Workspaces: workspaces,
		}
	case PipelineRunKind:
		pipeline := pipelineRunSpec{PipelineRef: reference{Name: config.DefinitionName}, Params: params, Workspaces: workspaces}
		pipeline.TaskRunTemplate.ServiceAccountName = config.ServiceAccountName
		pipeline.Timeouts.Pipeline = timeout
		spec = pipeline
	}
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return Resource{}, fmt.Errorf("marshal Tekton spec: %w", err)
	}
	submissionDigest := digest(request.SubmissionToken)
	taskDigest := digest(request.TaskID)
	name := resourceName(config.Kind, request.SubmissionToken)
	annotations := map[string]string{
		"atg-submission-token-sha256": submissionDigest,
		"atg-task-id":                 request.TaskID,
		"atg-attempt-id":              request.AttemptID,
		"atg-task-spec-digest":        request.TaskSpecDigest,
		"atg-profile-id":              request.ProfileID,
		"atg-profile-version":         request.ProfileVersion,
		"atg-profile-digest":          request.ProfileDigest,
		"atg-definition-digest":       config.DefinitionDigest,
		"atg-rendered-spec-sha256":    renderedSpecDigest(specBytes),
	}
	return Resource{
		APIVersion: APIVersion,
		Kind:       config.Kind,
		Metadata: ObjectMeta{
			Name: name, Namespace: config.Namespace,
			Labels: map[string]string{
				"atg-submission": submissionDigest[:63],
				"atg-task":       taskDigest[:63],
			},
			Annotations: annotations,
		},
		Spec: specBytes,
	}, nil
}

func Marshal(resource Resource) ([]byte, error) {
	data, err := json.MarshalIndent(resource, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal Tekton resource: %w", err)
	}
	return append(data, '\n'), nil
}

// ObservationFromResource normalizes Tekton state without treating a successful
// run condition as a test verdict. Observation sequence is assigned by the
// controller's durable claim fence; Tekton resourceVersion remains attributable
// provider metadata because Kubernetes defines it as opaque.
func ObservationFromResource(expected Resource, request provider.SubmitRequest, resource Resource, observedAt time.Time) (provider.Observation, error) {
	if err := request.Validate(); err != nil {
		return provider.Observation{}, err
	}
	if observedAt.IsZero() {
		return provider.Observation{}, provider.ErrObservationConflict
	}
	if err := VerifyResourceIdentity(expected, resource); err != nil {
		return provider.Observation{}, err
	}
	observation := provider.Observation{
		SubmissionToken: request.SubmissionToken, TaskID: request.TaskID, AttemptID: request.AttemptID,
		TaskSpecDigest: request.TaskSpecDigest, ProfileID: request.ProfileID, ProfileVersion: request.ProfileVersion,
		ProfileDigest: request.ProfileDigest, Job: provider.JobRef{ID: resource.Metadata.Namespace + "/" + resource.Metadata.Name + ":" + resource.Metadata.UID},
		ProviderRevision: resource.Metadata.ResourceVersion, ObservedAt: observedAt.UTC(),
	}
	condition := succeededCondition(resource.Status)
	cancelRequested := runCancellationRequested(resource.Spec)
	if cancelRequested {
		observation.Status = provider.StatusCancelRequested
	}
	if condition == nil {
		if !cancelRequested {
			observation.Status = pendingOrRunning(resource.Status)
		}
		return observation, nil
	}
	switch condition.Status {
	case "Unknown":
		if !cancelRequested {
			observation.Status = pendingOrRunning(resource.Status)
		}
		if strings.Contains(strings.ToLower(condition.Reason), "unschedul") {
			observation.FailureCategory = "worker_resource_exhausted"
		}
	case "True":
		if resource.Status != nil && resource.Status.CompletionTime != nil {
			observation.Status = provider.StatusCompleted
			observation.TerminationConfirmed = true
		} else {
			observation.Status = provider.StatusOutcomeUnknown
		}
	case "False":
		if isCancellationReason(condition.Reason) {
			if resource.Status != nil && resource.Status.CompletionTime != nil {
				observation.Status = provider.StatusCancelled
				observation.TerminationConfirmed = true
			} else {
				observation.Status = provider.StatusCancelRequested
			}
		} else if resource.Status != nil && resource.Status.CompletionTime != nil {
			observation.Status = provider.StatusFailed
			observation.FailureCategory = "unknown"
			observation.TerminationConfirmed = true
		} else {
			observation.Status = provider.StatusOutcomeUnknown
		}
	default:
		return provider.Observation{}, fmt.Errorf("unsupported Tekton condition status %q", condition.Status)
	}
	return observation, nil
}

func VerifyResourceIdentity(expected, actual Resource) error {
	if expected.APIVersion != actual.APIVersion || expected.Kind != actual.Kind ||
		expected.Metadata.Name != actual.Metadata.Name || expected.Metadata.Namespace != actual.Metadata.Namespace ||
		actual.Metadata.UID == "" || actual.Metadata.ResourceVersion == "" ||
		renderedSpecDigest(actual.Spec) != actual.Metadata.Annotations["atg-rendered-spec-sha256"] {
		return provider.ErrAttemptConflict
	}
	for key, value := range expected.Metadata.Annotations {
		if actual.Metadata.Annotations[key] != value {
			return provider.ErrAttemptConflict
		}
	}
	for key, value := range expected.Metadata.Labels {
		if actual.Metadata.Labels[key] != value {
			return provider.ErrAttemptConflict
		}
	}
	return nil
}

func resourceName(kind RunKind, token string) string {
	return "atg-" + strings.ToLower(string(kind)) + "-" + digest(token)[:24]
}

func succeededCondition(status *ResourceStatus) *Condition {
	if status == nil {
		return nil
	}
	for index := range status.Conditions {
		if status.Conditions[index].Type == "Succeeded" {
			return &status.Conditions[index]
		}
	}
	return nil
}

func pendingOrRunning(status *ResourceStatus) provider.Status {
	if status != nil && status.StartTime != nil {
		return provider.StatusRunning
	}
	return provider.StatusPending
}

func isCancellationReason(reason string) bool {
	switch strings.ToLower(reason) {
	case "taskruncancelled", "pipelineruncancelled", "cancelledrunfinally", "cancelled":
		return true
	default:
		return false
	}
}

func runCancellationRequested(spec json.RawMessage) bool {
	var value struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(spec, &value) == nil && strings.EqualFold(value.Status, "Cancelled")
}

func validateConfig(config Config) error {
	if (config.Kind != TaskRunKind && config.Kind != PipelineRunKind) ||
		!validDNSLabel(config.Namespace) || !validDNSLabel(config.DefinitionName) || !digestPattern.MatchString(config.DefinitionDigest) ||
		!validDNSLabel(config.ServiceAccountName) || !validDNSLabel(config.WorkspaceName) ||
		!quantityPattern.MatchString(config.WorkspaceSizeLimit) || config.TimeoutSeconds < 1 ||
		config.TimeoutSeconds > 86400 || config.NativeRetries != 0 {
		return ErrInvalidConfig
	}
	return nil
}

func validDNSLabel(value string) bool {
	return len(value) > 0 && len(value) <= 63 && dnsLabelPattern.MatchString(value)
}

func digest(value string) string { return digestBytes([]byte(value)) }

func digestBytes(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func renderedSpecDigest(data []byte) string {
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(data, &spec); err != nil || spec == nil {
		return ""
	}
	delete(spec, "status")
	canonical, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return digestBytes(canonical)
}
