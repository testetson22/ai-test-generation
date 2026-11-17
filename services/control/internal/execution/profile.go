package execution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

const MaxExecutionProfileBytes = 1 << 20

var (
	ErrInvalidExecutionProfile = errors.New("invalid execution profile")
	profileEnvPattern          = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	profileHostLabelPattern    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

type ExecutionProfile struct {
	Contract             string                       `json:"contract"`
	Version              string                       `json:"version"`
	ProfileID            string                       `json:"profile_id"`
	ProfileVersion       string                       `json:"profile_version"`
	Application          Identity                     `json:"application"`
	SourcePolicy         ProfileSourcePolicy          `json:"source_policy"`
	Target               ProfileTarget                `json:"target"`
	WorkerImage          Identity                     `json:"worker_image"`
	Commands             []ProfileCommand             `json:"commands"`
	TestSelection        ProfileTestSelection         `json:"test_selection"`
	ApplicationLifecycle *ProfileApplicationLifecycle `json:"application_lifecycle,omitempty"`
	Limits               ProfileLimits                `json:"limits"`
	NetworkPolicy        ProfileNetworkPolicy         `json:"network_policy"`
	CredentialPolicy     ProfileCredentialPolicy      `json:"credential_policy"`
}

type ProfileSourcePolicy struct {
	Repository         string   `json:"repository"`
	RevisionPolicy     string   `json:"revision_policy"`
	AllowedSourceRoots []string `json:"allowed_source_roots,omitempty"`
}

type ProfileTarget struct {
	Language         string   `json:"language"`
	Framework        string   `json:"framework"`
	FrameworkVersion string   `json:"framework_version"`
	Runtime          Identity `json:"runtime"`
	BuildTool        Identity `json:"build_tool"`
}

type ProfileCommand struct {
	CommandID        string   `json:"command_id"`
	Phase            string   `json:"phase"`
	Argv             []string `json:"argv"`
	WorkingDirectory string   `json:"working_directory"`
	TimeoutSeconds   int      `json:"timeout_seconds"`
	EnvironmentNames []string `json:"environment_names,omitempty"`
}

type ProfileTestSelection struct {
	CommandID        string `json:"command_id"`
	Selector         string `json:"selector"`
	ReportMediaType  string `json:"report_media_type"`
	MinimumTestCount int    `json:"minimum_test_count"`
}

type ProfileApplicationLifecycle struct {
	StartCommandID       string `json:"start_command_id,omitempty"`
	HealthCheckCommandID string `json:"health_check_command_id,omitempty"`
	ResetCommandID       string `json:"reset_command_id,omitempty"`
	CleanupCommandID     string `json:"cleanup_command_id,omitempty"`
}

type ProfileLimits struct {
	WallTimeoutSeconds int64 `json:"wall_timeout_seconds"`
	CPUMillis          int64 `json:"cpu_millis"`
	MemoryBytes        int64 `json:"memory_bytes"`
	MaxPIDs            int64 `json:"max_pids"`
	MaxOutputBytes     int64 `json:"max_output_bytes"`
	MaxArtifactBytes   int64 `json:"max_artifact_bytes"`
}

type ProfileNetworkPolicy struct {
	Mode         string   `json:"mode"`
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

type ProfileCredentialPolicy struct {
	WorkloadCredentials       string   `json:"workload_credentials"`
	AcquisitionCredentialRefs []string `json:"acquisition_credential_refs"`
}

// DecodeBoundProfile validates the profile bytes against the immutable task
// reference and embedded approval digest. It does not authenticate the reviewer;
// dispatch admission remains responsible for that authority check.
func DecodeBoundProfile(spec TaskSpec, data []byte) (ExecutionProfile, error) {
	if err := spec.Validate(); err != nil {
		return ExecutionProfile{}, fmt.Errorf("task specification: %w", err)
	}
	if len(data) == 0 || len(data) > MaxExecutionProfileBytes || DigestTaskSpec(data) != spec.Profile.Artifact.Digest || spec.ProfileApproval.ApprovedProfileDigest != spec.Profile.Artifact.Digest || spec.Profile.Artifact.SizeBytes > 0 && spec.Profile.Artifact.SizeBytes != int64(len(data)) {
		return ExecutionProfile{}, fmt.Errorf("%w: profile bytes do not match bounded approved artifact digest", ErrInvalidExecutionProfile)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var profile ExecutionProfile
	if err := decoder.Decode(&profile); err != nil {
		return ExecutionProfile{}, fmt.Errorf("%w: decode profile: %v", ErrInvalidExecutionProfile, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ExecutionProfile{}, fmt.Errorf("%w: document must contain exactly one JSON value", ErrInvalidExecutionProfile)
	}
	if profile.ProfileID != spec.Profile.ProfileID || profile.ProfileVersion != spec.Profile.ProfileVersion {
		return ExecutionProfile{}, fmt.Errorf("%w: profile identity differs from the task reference", ErrInvalidExecutionProfile)
	}
	if err := profile.Validate(); err != nil {
		return ExecutionProfile{}, err
	}
	return profile, nil
}

func (p ExecutionProfile) Validate() error {
	if p.Contract != "atg.execution_profile" || p.Version != "1.0" || !validID(p.ProfileID) || !semverPattern.MatchString(p.ProfileVersion) {
		return fmt.Errorf("%w: contract or profile identity is invalid", ErrInvalidExecutionProfile)
	}
	if err := validateIdentity(p.Application, false); err != nil {
		return fmt.Errorf("%w: application identity: %v", ErrInvalidExecutionProfile, err)
	}
	repository, err := url.Parse(p.SourcePolicy.Repository)
	if err != nil || repository.Scheme != "https" || repository.Host == "" || repository.User != nil || repository.RawQuery != "" || repository.Fragment != "" || p.SourcePolicy.RevisionPolicy != "immutable" {
		return fmt.Errorf("%w: source policy must pin an immutable credential-free HTTPS repository", ErrInvalidExecutionProfile)
	}
	if err := validatePathRoots(p.SourcePolicy.AllowedSourceRoots); err != nil {
		return err
	}
	if strings.TrimSpace(p.Target.Language) == "" || strings.TrimSpace(p.Target.Framework) == "" || strings.TrimSpace(p.Target.FrameworkVersion) == "" {
		return fmt.Errorf("%w: application target identity is incomplete", ErrInvalidExecutionProfile)
	}
	if err := validateIdentity(p.Target.Runtime, false); err != nil {
		return fmt.Errorf("%w: runtime identity: %v", ErrInvalidExecutionProfile, err)
	}
	if err := validateIdentity(p.Target.BuildTool, false); err != nil {
		return fmt.Errorf("%w: build tool identity: %v", ErrInvalidExecutionProfile, err)
	}
	if err := validateIdentity(p.WorkerImage, true); err != nil {
		return fmt.Errorf("%w: worker image identity: %v", ErrInvalidExecutionProfile, err)
	}
	if len(p.Commands) == 0 || len(p.Commands) > 32 {
		return fmt.Errorf("%w: command count must be between 1 and 32", ErrInvalidExecutionProfile)
	}
	commands := make(map[string]ProfileCommand, len(p.Commands))
	totalArgBytes := 0
	for _, command := range p.Commands {
		if !validID(command.CommandID) || !validProfilePhase(command.Phase) || len(command.Argv) == 0 || len(command.Argv) > 64 || command.TimeoutSeconds < 1 || command.TimeoutSeconds > 86400 {
			return fmt.Errorf("%w: command identity, phase, argv count, or timeout is invalid", ErrInvalidExecutionProfile)
		}
		if _, exists := commands[command.CommandID]; exists {
			return fmt.Errorf("%w: command IDs must be unique", ErrInvalidExecutionProfile)
		}
		if err := validateRelativeWorkingDirectory(command.WorkingDirectory); err != nil {
			return err
		}
		seenEnvironment := make(map[string]struct{}, len(command.EnvironmentNames))
		for _, name := range command.EnvironmentNames {
			if !profileEnvPattern.MatchString(name) {
				return fmt.Errorf("%w: invalid command environment name", ErrInvalidExecutionProfile)
			}
			if _, exists := seenEnvironment[name]; exists {
				return fmt.Errorf("%w: command environment names must be unique", ErrInvalidExecutionProfile)
			}
			seenEnvironment[name] = struct{}{}
		}
		for _, arg := range command.Argv {
			if arg == "" || strings.ContainsRune(arg, '\x00') || len(arg) > 4096 {
				return fmt.Errorf("%w: empty, oversized, or NUL-containing argv element", ErrInvalidExecutionProfile)
			}
			totalArgBytes += len(arg)
			if totalArgBytes > 65536 {
				return fmt.Errorf("%w: aggregate argv bytes exceed limit", ErrInvalidExecutionProfile)
			}
		}
		commands[command.CommandID] = command
	}
	selectionCommand, exists := commands[p.TestSelection.CommandID]
	if !exists || selectionCommand.Phase != "test" || strings.TrimSpace(p.TestSelection.Selector) == "" || len(p.TestSelection.Selector) > 4096 || strings.TrimSpace(p.TestSelection.ReportMediaType) == "" || len(p.TestSelection.ReportMediaType) > 255 || p.TestSelection.MinimumTestCount < 1 {
		return fmt.Errorf("%w: test selection must bind a test command, selector, report type, and positive minimum", ErrInvalidExecutionProfile)
	}
	if p.ApplicationLifecycle != nil {
		for _, lifecycleCommand := range []struct{ commandID, phase string }{
			{p.ApplicationLifecycle.StartCommandID, "application_start"},
			{p.ApplicationLifecycle.ResetCommandID, "application_reset"},
			{p.ApplicationLifecycle.CleanupCommandID, "cleanup"},
		} {
			if lifecycleCommand.commandID == "" {
				continue
			}
			command, exists := commands[lifecycleCommand.commandID]
			if !exists || command.Phase != lifecycleCommand.phase {
				return fmt.Errorf("%w: lifecycle command %q has the wrong phase", ErrInvalidExecutionProfile, lifecycleCommand.commandID)
			}
		}
		if commandID := p.ApplicationLifecycle.HealthCheckCommandID; commandID != "" {
			if _, exists := commands[commandID]; !exists {
				return fmt.Errorf("%w: health-check command is not declared", ErrInvalidExecutionProfile)
			}
		}
	}
	limits := p.Limits
	if limits.WallTimeoutSeconds < 1 || limits.WallTimeoutSeconds > 86400 || limits.CPUMillis < 1 || limits.MemoryBytes < 1 || limits.MaxPIDs < 1 || limits.MaxOutputBytes < 1 || limits.MaxArtifactBytes < 1 {
		return fmt.Errorf("%w: resource and output limits must be positive and bounded", ErrInvalidExecutionProfile)
	}
	switch p.NetworkPolicy.Mode {
	case "disabled":
		if len(p.NetworkPolicy.AllowedHosts) != 0 {
			return fmt.Errorf("%w: disabled network policy cannot allow hosts", ErrInvalidExecutionProfile)
		}
	case "allowlist":
		if len(p.NetworkPolicy.AllowedHosts) == 0 || len(p.NetworkPolicy.AllowedHosts) > 64 {
			return fmt.Errorf("%w: network allowlist must contain 1..64 hosts", ErrInvalidExecutionProfile)
		}
		seenHosts := make(map[string]struct{}, len(p.NetworkPolicy.AllowedHosts))
		for _, host := range p.NetworkPolicy.AllowedHosts {
			if !validProfileHost(host) {
				return fmt.Errorf("%w: invalid network host", ErrInvalidExecutionProfile)
			}
			if _, exists := seenHosts[host]; exists {
				return fmt.Errorf("%w: network hosts must be unique", ErrInvalidExecutionProfile)
			}
			seenHosts[host] = struct{}{}
		}
	default:
		return fmt.Errorf("%w: network mode must be disabled or allowlist", ErrInvalidExecutionProfile)
	}
	if p.CredentialPolicy.WorkloadCredentials != "none" || p.CredentialPolicy.AcquisitionCredentialRefs == nil || !uniqueNonEmpty(p.CredentialPolicy.AcquisitionCredentialRefs) {
		return fmt.Errorf("%w: workload credentials are forbidden and acquisition references must be unique", ErrInvalidExecutionProfile)
	}
	for _, ref := range p.CredentialPolicy.AcquisitionCredentialRefs {
		if !validID(ref) {
			return fmt.Errorf("%w: invalid acquisition credential reference", ErrInvalidExecutionProfile)
		}
	}
	return nil
}

func validProfileHost(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if !profileHostLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func validatePathRoots(roots []string) error {
	if len(roots) > 64 {
		return fmt.Errorf("%w: source root count exceeds limit", ErrInvalidExecutionProfile)
	}
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if root == "" || strings.ContainsAny(root, "\\\x00") || path.IsAbs(root) || path.Clean(root) != root || root == ".." || strings.HasPrefix(root, "../") {
			return fmt.Errorf("%w: source roots must be normalized relative paths", ErrInvalidExecutionProfile)
		}
		if _, exists := seen[root]; exists {
			return fmt.Errorf("%w: source roots must be unique", ErrInvalidExecutionProfile)
		}
		seen[root] = struct{}{}
	}
	return nil
}

func validateRelativeWorkingDirectory(value string) error {
	if value == "" || strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("%w: working directory must be a normalized relative path", ErrInvalidExecutionProfile)
	}
	return nil
}

func validProfilePhase(phase string) bool {
	return slices.Contains([]string{"prepare", "build", "discover", "test", "collect", "application_start", "application_reset", "cleanup"}, phase)
}
