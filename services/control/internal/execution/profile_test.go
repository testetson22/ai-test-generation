package execution

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeBoundProfileUsesOnlyDigestBoundSyntheticProfile(t *testing.T) {
	spec, data := bindSyntheticProfile(t, syntheticProfile())
	decoded, err := DecodeBoundProfile(spec, data)
	if err != nil {
		t.Fatalf("decode synthetic bound profile: %v", err)
	}
	if decoded.ProfileID != "unit-fixture-profile" || decoded.TestSelection.CommandID != "unit-test-command" {
		t.Fatalf("decoded synthetic profile = %#v", decoded)
	}
}

func TestDecodeBoundProfileRejectsDigestIdentityAndUnknownFieldDrift(t *testing.T) {
	t.Run("artifact byte drift", func(t *testing.T) {
		spec, data := bindSyntheticProfile(t, syntheticProfile())
		data = append(data, ' ')
		if _, err := DecodeBoundProfile(spec, data); !errors.Is(err, ErrInvalidExecutionProfile) {
			t.Fatalf("changed profile bytes error = %v", err)
		}
	})

	t.Run("profile identity mismatch", func(t *testing.T) {
		spec, data := bindSyntheticProfile(t, syntheticProfile())
		spec.Profile.ProfileID = "another-profile"
		if _, err := DecodeBoundProfile(spec, data); !errors.Is(err, ErrInvalidExecutionProfile) {
			t.Fatalf("profile identity mismatch error = %v", err)
		}
	})

	t.Run("artifact size mismatch", func(t *testing.T) {
		spec, data := bindSyntheticProfile(t, syntheticProfile())
		spec.Profile.Artifact.SizeBytes++
		if _, err := DecodeBoundProfile(spec, data); !errors.Is(err, ErrInvalidExecutionProfile) {
			t.Fatalf("profile size mismatch error = %v", err)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		profile := syntheticProfile()
		encoded, err := json.Marshal(profile)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		object["unreviewed_command"] = []string{"sh", "-c", "anything"}
		encoded, err = json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		spec := bindProfileDigest(t, profile.ProfileID, profile.ProfileVersion, encoded)
		if _, err := DecodeBoundProfile(spec, encoded); !errors.Is(err, ErrInvalidExecutionProfile) || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("unknown profile field error = %v", err)
		}
	})
}

func TestDecodeBoundProfileRejectsUnsafeCommandsAndCredentialPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ExecutionProfile)
	}{
		{name: "path traversal", mutate: func(profile *ExecutionProfile) { profile.Commands[0].WorkingDirectory = "../outside" }},
		{name: "mismatched test phase", mutate: func(profile *ExecutionProfile) { profile.Commands[0].Phase = "build" }},
		{name: "workload credentials", mutate: func(profile *ExecutionProfile) { profile.CredentialPolicy.WorkloadCredentials = "available" }},
		{name: "network wildcard", mutate: func(profile *ExecutionProfile) {
			profile.NetworkPolicy = ProfileNetworkPolicy{Mode: "allowlist", AllowedHosts: []string{"*.example.com"}}
		}},
		{name: "missing acquisition reference inventory", mutate: func(profile *ExecutionProfile) {
			profile.CredentialPolicy.AcquisitionCredentialRefs = nil
		}},
		{name: "lifecycle phase drift", mutate: func(profile *ExecutionProfile) {
			profile.Commands = append(profile.Commands, ProfileCommand{CommandID: "bad-cleanup", Phase: "test", Argv: []string{"fixture-cleanup"}, WorkingDirectory: ".", TimeoutSeconds: 10})
			profile.ApplicationLifecycle = &ProfileApplicationLifecycle{CleanupCommandID: "bad-cleanup"}
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			profile := syntheticProfile()
			testCase.mutate(&profile)
			spec, data := bindSyntheticProfile(t, profile)
			if _, err := DecodeBoundProfile(spec, data); !errors.Is(err, ErrInvalidExecutionProfile) {
				t.Fatalf("invalid profile error = %v", err)
			}
		})
	}
}

func bindSyntheticProfile(t *testing.T, profile ExecutionProfile) (TaskSpec, []byte) {
	t.Helper()
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal synthetic profile: %v", err)
	}
	return bindProfileDigest(t, profile.ProfileID, profile.ProfileVersion, data), data
}

func bindProfileDigest(t *testing.T, profileID, version string, data []byte) TaskSpec {
	t.Helper()
	spec := validTaskSpec()
	digest := DigestTaskSpec(data)
	spec.Profile.ProfileID = profileID
	spec.Profile.ProfileVersion = version
	spec.Profile.Artifact.Digest = digest
	spec.Profile.Artifact.SizeBytes = int64(len(data))
	spec.ProfileApproval.ApprovedProfileDigest = digest
	if err := spec.Validate(); err != nil {
		t.Fatalf("bind synthetic task profile: %v", err)
	}
	return spec
}

func syntheticProfile() ExecutionProfile {
	return ExecutionProfile{
		Contract: "atg.execution_profile", Version: "1.0", ProfileID: "unit-fixture-profile", ProfileVersion: "1.2.3",
		Application:  Identity{Kind: "fixture-application", ID: "unit-app"},
		SourcePolicy: ProfileSourcePolicy{Repository: "https://example.invalid/unit.git", RevisionPolicy: "immutable", AllowedSourceRoots: []string{"src", "tests"}},
		Target: ProfileTarget{
			Language: "fixture", Framework: "unit", FrameworkVersion: "1",
			Runtime:   Identity{Kind: "runtime", ID: "unit-runtime", Version: "1"},
			BuildTool: Identity{Kind: "build-tool", ID: "unit-tool", Version: "1"},
		},
		WorkerImage: Identity{Kind: "oci-image", ID: "unit-worker", Version: "1", Digest: "sha256:" + strings.Repeat("c", 64)},
		Commands: []ProfileCommand{{
			CommandID: "unit-test-command", Phase: "test", Argv: []string{"unit-runner", "--selection", "Smoke"},
			WorkingDirectory: "workspace", TimeoutSeconds: 30, EnvironmentNames: []string{"UNIT_MODE"},
		}},
		TestSelection:    ProfileTestSelection{CommandID: "unit-test-command", Selector: "Smoke", ReportMediaType: "application/vnd.fixture.report+json", MinimumTestCount: 1},
		Limits:           ProfileLimits{WallTimeoutSeconds: 60, CPUMillis: 1000, MemoryBytes: 1 << 30, MaxPIDs: 32, MaxOutputBytes: 1 << 20, MaxArtifactBytes: 1 << 20},
		NetworkPolicy:    ProfileNetworkPolicy{Mode: "disabled", AllowedHosts: []string{}},
		CredentialPolicy: ProfileCredentialPolicy{WorkloadCredentials: "none", AcquisitionCredentialRefs: []string{}},
	}
}
