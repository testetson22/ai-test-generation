package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const MaxTaskSpecBytes = 1 << 20

var (
	ErrInvalidTaskSpec = errors.New("invalid execution task specification")
	idPattern          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	digestPattern      = regexp.MustCompile(`^sha256:[A-Fa-f0-9]{64}$`)
	semverPattern      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

type Identity struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

type Artifact struct {
	ArtifactID string `json:"artifact_id"`
	MediaType  string `json:"media_type"`
	Digest     string `json:"digest"`
	URI        string `json:"uri,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
}

type Source struct {
	Identity      Identity `json:"identity"`
	Locator       string   `json:"locator"`
	RetrievedAt   string   `json:"retrieved_at,omitempty"`
	ContentDigest string   `json:"content_digest,omitempty"`
}

type Reviewer struct {
	Identity         Identity `json:"identity"`
	Role             string   `json:"role"`
	AuthorizationRef string   `json:"authorization_ref"`
	AuthenticatedAt  string   `json:"authenticated_at,omitempty"`
}

type GenerationRefs struct {
	Manifest Artifact `json:"manifest"`
	Patch    Artifact `json:"patch"`
	Tests    Artifact `json:"tests"`
}

type CodeApproval struct {
	CheckpointID             string   `json:"checkpoint_id"`
	DecisionID               string   `json:"decision_id"`
	Reviewer                 Reviewer `json:"reviewer"`
	ReviewedGenerationDigest string   `json:"reviewed_generation_digest"`
}

type ProfileRef struct {
	ProfileID      string   `json:"profile_id"`
	ProfileVersion string   `json:"profile_version"`
	Artifact       Artifact `json:"artifact"`
}

type ProfileApproval struct {
	DecisionID            string   `json:"decision_id"`
	Reviewer              Reviewer `json:"reviewer"`
	ApprovedProfileDigest string   `json:"approved_profile_digest"`
}

type RetryPolicy struct {
	PolicyID              string   `json:"policy_id"`
	MaxAttempts           int      `json:"max_attempts"`
	DeadlineAt            string   `json:"deadline_at"`
	InitialBackoffSeconds int64    `json:"initial_backoff_seconds"`
	MaxBackoffSeconds     int64    `json:"max_backoff_seconds"`
	RetryableFailures     []string `json:"retryable_failures"`
}

type TaskSpec struct {
	Contract            string          `json:"contract"`
	Version             string          `json:"version"`
	TaskSpecID          string          `json:"task_spec_id"`
	RunID               string          `json:"run_id"`
	ExpectedRunRevision int64           `json:"expected_run_revision"`
	PairID              string          `json:"pair_id"`
	GenerationID        string          `json:"generation_id"`
	SubjectRole         string          `json:"subject_role"`
	ComparisonMode      string          `json:"comparison_mode"`
	Subject             Subject         `json:"subject"`
	Generation          GenerationRefs  `json:"generation"`
	CodeApproval        CodeApproval    `json:"code_approval"`
	Profile             ProfileRef      `json:"profile"`
	ProfileApproval     ProfileApproval `json:"profile_approval"`
	ExpectedTestIDs     []string        `json:"expected_test_ids"`
	RetryPolicy         RetryPolicy     `json:"retry_policy"`
	CreatedAt           string          `json:"created_at"`
}

type Subject struct {
	Source   Source   `json:"source"`
	Identity Identity `json:"identity"`
}

func DecodeTaskSpec(data []byte) (TaskSpec, error) {
	if len(data) == 0 || len(data) > MaxTaskSpecBytes {
		return TaskSpec{}, fmt.Errorf("%w: encoded size must be between 1 and %d bytes", ErrInvalidTaskSpec, MaxTaskSpecBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var spec TaskSpec
	if err := decoder.Decode(&spec); err != nil {
		return TaskSpec{}, fmt.Errorf("%w: %v", ErrInvalidTaskSpec, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return TaskSpec{}, fmt.Errorf("%w: document must contain exactly one JSON value", ErrInvalidTaskSpec)
	}
	if err := spec.Validate(); err != nil {
		return TaskSpec{}, err
	}
	return spec, nil
}

func (s TaskSpec) Validate() error {
	if s.Contract != "atg.execution_task_spec" || s.Version != "1.0" {
		return fmt.Errorf("%w: unsupported contract or version", ErrInvalidTaskSpec)
	}
	for _, field := range []struct{ name, value string }{
		{name: "task_spec_id", value: s.TaskSpecID},
		{name: "run_id", value: s.RunID},
		{name: "pair_id", value: s.PairID},
		{name: "generation_id", value: s.GenerationID},
	} {
		if !validID(field.value) {
			return fmt.Errorf("%w: %s is invalid", ErrInvalidTaskSpec, field.name)
		}
	}
	if s.ExpectedRunRevision < 1 {
		return fmt.Errorf("%w: expected_run_revision must be positive", ErrInvalidTaskSpec)
	}
	if s.SubjectRole != "base" && s.SubjectRole != "candidate" {
		return fmt.Errorf("%w: subject_role must be base or candidate", ErrInvalidTaskSpec)
	}
	if s.ComparisonMode != "same_subject_control" && s.ComparisonMode != "differential" {
		return fmt.Errorf("%w: unsupported comparison_mode", ErrInvalidTaskSpec)
	}
	if err := validateIdentity(s.Subject.Source.Identity, true); err != nil {
		return fmt.Errorf("%w: source identity: %v", ErrInvalidTaskSpec, err)
	}
	if err := validateIdentity(s.Subject.Identity, true); err != nil {
		return fmt.Errorf("%w: subject identity: %v", ErrInvalidTaskSpec, err)
	}
	locator, err := url.Parse(s.Subject.Source.Locator)
	if err != nil || locator.Scheme != "https" || locator.Host == "" || locator.User != nil || locator.RawQuery != "" || locator.Fragment != "" {
		return fmt.Errorf("%w: source locator must be an https URI without embedded credentials", ErrInvalidTaskSpec)
	}
	for name, artifact := range map[string]Artifact{
		"generation manifest": s.Generation.Manifest,
		"patch":               s.Generation.Patch,
		"tests":               s.Generation.Tests,
		"profile":             s.Profile.Artifact,
	} {
		if err := validateArtifact(artifact); err != nil {
			return fmt.Errorf("%w: %s artifact: %v", ErrInvalidTaskSpec, name, err)
		}
	}
	if !validID(s.CodeApproval.CheckpointID) || !validID(s.CodeApproval.DecisionID) || !validReviewer(s.CodeApproval.Reviewer) || !validDigest(s.CodeApproval.ReviewedGenerationDigest) {
		return fmt.Errorf("%w: code approval is incomplete", ErrInvalidTaskSpec)
	}
	if s.CodeApproval.ReviewedGenerationDigest != s.Generation.Manifest.Digest {
		return fmt.Errorf("%w: code approval does not bind the generation manifest", ErrInvalidTaskSpec)
	}
	if !validID(s.Profile.ProfileID) || !semverPattern.MatchString(s.Profile.ProfileVersion) || !validID(s.ProfileApproval.DecisionID) || !validReviewer(s.ProfileApproval.Reviewer) {
		return fmt.Errorf("%w: approved profile reference is incomplete", ErrInvalidTaskSpec)
	}
	if !validDigest(s.ProfileApproval.ApprovedProfileDigest) || s.ProfileApproval.ApprovedProfileDigest != s.Profile.Artifact.Digest {
		return fmt.Errorf("%w: profile approval does not bind the selected profile artifact", ErrInvalidTaskSpec)
	}
	if len(s.ExpectedTestIDs) == 0 {
		return fmt.Errorf("%w: expected_test_ids must not be empty", ErrInvalidTaskSpec)
	}
	seenTests := make(map[string]struct{}, len(s.ExpectedTestIDs))
	for _, testID := range s.ExpectedTestIDs {
		if strings.TrimSpace(testID) == "" || strings.TrimSpace(testID) != testID {
			return fmt.Errorf("%w: expected test ID must not be empty", ErrInvalidTaskSpec)
		}
		if _, exists := seenTests[testID]; exists {
			return fmt.Errorf("%w: expected test IDs must be unique", ErrInvalidTaskSpec)
		}
		seenTests[testID] = struct{}{}
	}
	if !validID(s.RetryPolicy.PolicyID) || s.RetryPolicy.MaxAttempts < 1 || s.RetryPolicy.MaxAttempts > 10 || s.RetryPolicy.InitialBackoffSeconds < 0 || s.RetryPolicy.MaxBackoffSeconds > 86400 || s.RetryPolicy.MaxBackoffSeconds < s.RetryPolicy.InitialBackoffSeconds {
		return fmt.Errorf("%w: retry policy bounds are invalid", ErrInvalidTaskSpec)
	}
	if _, err := time.Parse(time.RFC3339, s.RetryPolicy.DeadlineAt); err != nil {
		return fmt.Errorf("%w: retry deadline must be RFC3339", ErrInvalidTaskSpec)
	}
	if _, err := time.Parse(time.RFC3339, s.CreatedAt); err != nil {
		return fmt.Errorf("%w: created_at must be RFC3339", ErrInvalidTaskSpec)
	}
	allowedFailures := map[string]struct{}{
		"provider_unavailable": {}, "worker_lost": {},
		"worker_resource_exhausted": {}, "artifact_transport_failed": {},
	}
	seenFailures := make(map[string]struct{}, len(s.RetryPolicy.RetryableFailures))
	for _, failure := range s.RetryPolicy.RetryableFailures {
		if _, ok := allowedFailures[failure]; !ok {
			return fmt.Errorf("%w: failure category %q is not retryable", ErrInvalidTaskSpec, failure)
		}
		if _, exists := seenFailures[failure]; exists {
			return fmt.Errorf("%w: retryable failure categories must be unique", ErrInvalidTaskSpec)
		}
		seenFailures[failure] = struct{}{}
	}
	return nil
}

func ValidatePair(base, candidate TaskSpec) error {
	if err := base.Validate(); err != nil {
		return err
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	if base.SubjectRole != "base" || candidate.SubjectRole != "candidate" {
		return fmt.Errorf("%w: pair must contain exactly one base and one candidate", ErrInvalidTaskSpec)
	}
	if base.TaskSpecID == candidate.TaskSpecID {
		return fmt.Errorf("%w: base and candidate task specification IDs must differ", ErrInvalidTaskSpec)
	}
	if base.RunID != candidate.RunID || base.ExpectedRunRevision != candidate.ExpectedRunRevision || base.PairID != candidate.PairID || base.GenerationID != candidate.GenerationID || base.ComparisonMode != candidate.ComparisonMode {
		return fmt.Errorf("%w: pair run, generation, and comparison identities must match", ErrInvalidTaskSpec)
	}
	if base.Subject.Source.Identity.Digest == "" || candidate.Subject.Source.Identity.Digest == "" {
		return fmt.Errorf("%w: pair sources must have immutable digests", ErrInvalidTaskSpec)
	}
	if base.Generation != candidate.Generation || base.CodeApproval != candidate.CodeApproval || base.Profile != candidate.Profile || base.ProfileApproval != candidate.ProfileApproval || !slices.Equal(base.ExpectedTestIDs, candidate.ExpectedTestIDs) || !retryPoliciesEqual(base.RetryPolicy, candidate.RetryPolicy) {
		return fmt.Errorf("%w: pair must use the same approved generation, profile, tests, and retry policy", ErrInvalidTaskSpec)
	}
	sameSubject := base.Subject.Identity.Digest == candidate.Subject.Identity.Digest && base.Subject.Source.Identity.Digest == candidate.Subject.Source.Identity.Digest
	if base.ComparisonMode == "same_subject_control" && !sameSubject {
		return fmt.Errorf("%w: same-subject control must use identical immutable subjects", ErrInvalidTaskSpec)
	}
	if base.ComparisonMode == "differential" && sameSubject {
		return fmt.Errorf("%w: differential comparison requires distinct immutable subjects", ErrInvalidTaskSpec)
	}
	return nil
}

func DigestTaskSpec(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validateIdentity(identity Identity, requireDigest bool) error {
	if identity.Kind == "" || !validID(identity.ID) {
		return errors.New("identity kind and ID are required")
	}
	if requireDigest && !validDigest(identity.Digest) {
		return errors.New("immutable identity requires a SHA-256 digest")
	}
	if identity.Digest != "" && !validDigest(identity.Digest) {
		return errors.New("identity digest must be SHA-256")
	}
	return nil
}

func validateArtifact(artifact Artifact) error {
	if !validID(artifact.ArtifactID) || artifact.MediaType == "" || !validDigest(artifact.Digest) || artifact.SizeBytes < 0 {
		return errors.New("artifact identity, media type, digest, and nonnegative size are required")
	}
	if artifact.URI != "" {
		locator, err := url.Parse(artifact.URI)
		if err != nil || (locator.Scheme != "https" && locator.Scheme != "s3") || locator.Host == "" || locator.User != nil || locator.RawQuery != "" || locator.Fragment != "" {
			return errors.New("artifact URI must be a stable credential-free https or s3 reference")
		}
	}
	return nil
}

func validReviewer(reviewer Reviewer) bool {
	return validateIdentity(reviewer.Identity, false) == nil && reviewer.Role != "" && validID(reviewer.AuthorizationRef)
}

func validID(value string) bool {
	return idPattern.MatchString(value)
}

func validDigest(value string) bool {
	return digestPattern.MatchString(value)
}

func retryPoliciesEqual(left, right RetryPolicy) bool {
	return left.PolicyID == right.PolicyID && left.MaxAttempts == right.MaxAttempts && left.DeadlineAt == right.DeadlineAt && left.InitialBackoffSeconds == right.InitialBackoffSeconds && left.MaxBackoffSeconds == right.MaxBackoffSeconds && slices.Equal(left.RetryableFailures, right.RetryableFailures)
}
