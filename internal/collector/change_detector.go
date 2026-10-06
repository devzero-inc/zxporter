package collector

import (
	"github.com/go-logr/logr"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ChangeDetectionStatus string

const (
	// IgnoreChanges means the object did not move at all (same resourceVersion).
	IgnoreChanges ChangeDetectionStatus = "IgnoreChanges"

	// PushChanges means the ObjectMeta itself changed and the update must be sent.
	PushChanges ChangeDetectionStatus = "PushChanges"

	// UnknownChanges means the object moved but the ObjectMeta alone cannot say
	// whether it moved in a way we care about. Callers MUST fall through to
	// their own spec and status comparisons on this value:
	//
	//	if changed == PushChanges {
	//		return true
	//	}
	//	// spec/status comparisons follow
	//
	// Returning "changed == PushChanges" for every non-IgnoreChanges value
	// instead silently drops every status-only update — a rollout advancing,
	// ResourceQuota usage moving, a Job finishing — because those bump
	// resourceVersion without touching labels or annotations, which left the
	// spec and status comparisons below unreachable. That was a real bug.
	UnknownChanges ChangeDetectionStatus = "UnknownChanges"
)

type ChangeDetectionHelper struct {
	logger logr.Logger
}

func (c *ChangeDetectionHelper) objectMetaChanged(
	kind string,
	resourceName string,
	old v1.ObjectMeta,
	new v1.ObjectMeta,
) ChangeDetectionStatus {
	if old.ResourceVersion == new.ResourceVersion {
		return IgnoreChanges
	}

	// Check for label changes
	if !mapsEqual(old.Labels, new.Labels) {
		return PushChanges
	}

	// Check for annotation changes
	if !mapsEqual(old.Annotations, new.Annotations) {
		return PushChanges
	}

	return UnknownChanges
}
