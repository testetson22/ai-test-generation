package testkit

import "errors"

var (
	ErrAuthorizationDenied = errors.New("dispatch authorization denied")
	ErrProfileNotApproved  = errors.New("execution profile is not approved")
	ErrCapabilityMissing   = errors.New("required execution capability unavailable")
	ErrDependencyNotReady  = errors.New("required service dependency is not ready")
	ErrCapacityUnavailable = errors.New("execution capacity unavailable")
)

type DispatchPrerequisites struct {
	AuthorizationValid bool
	ProfileDigest      string
	ApprovedDigest     string
	CapabilityReady    bool
	DependenciesReady  bool
	CapacityAvailable  bool
}

func (p DispatchPrerequisites) Check() error {
	if !p.AuthorizationValid {
		return ErrAuthorizationDenied
	}
	if p.ProfileDigest == "" || p.ProfileDigest != p.ApprovedDigest {
		return ErrProfileNotApproved
	}
	if !p.CapabilityReady {
		return ErrCapabilityMissing
	}
	if !p.DependenciesReady {
		return ErrDependencyNotReady
	}
	if !p.CapacityAvailable {
		return ErrCapacityUnavailable
	}
	return nil
}
