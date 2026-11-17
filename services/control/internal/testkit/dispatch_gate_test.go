package testkit

import (
	"errors"
	"testing"
)

func TestDispatchPrerequisitesAreRevalidated(t *testing.T) {
	valid := DispatchPrerequisites{
		AuthorizationValid: true,
		ProfileDigest:      "sha256:approved-profile",
		ApprovedDigest:     "sha256:approved-profile",
		CapabilityReady:    true,
		DependenciesReady:  true,
		CapacityAvailable:  true,
	}
	if err := valid.Check(); err != nil {
		t.Fatalf("valid prerequisites: %v", err)
	}

	cases := []struct {
		name string
		edit func(*DispatchPrerequisites)
		want error
	}{
		{name: "authorization revoked", edit: func(p *DispatchPrerequisites) { p.AuthorizationValid = false }, want: ErrAuthorizationDenied},
		{name: "profile changed", edit: func(p *DispatchPrerequisites) { p.ProfileDigest = "sha256:stale-profile" }, want: ErrProfileNotApproved},
		{name: "capability lost", edit: func(p *DispatchPrerequisites) { p.CapabilityReady = false }, want: ErrCapabilityMissing},
		{name: "dependency unavailable", edit: func(p *DispatchPrerequisites) { p.DependenciesReady = false }, want: ErrDependencyNotReady},
		{name: "capacity exhausted", edit: func(p *DispatchPrerequisites) { p.CapacityAvailable = false }, want: ErrCapacityUnavailable},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			current := valid
			test.edit(&current)
			if err := current.Check(); !errors.Is(err, test.want) {
				t.Fatalf("check error = %v, want %v", err, test.want)
			}
		})
	}
}
