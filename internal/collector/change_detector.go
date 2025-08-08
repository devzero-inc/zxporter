package collector

import (
	"github.com/go-logr/logr"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ChangeDetectionStatus string

const (
	IgnoreChanges  ChangeDetectionStatus = "IgnoreChanges"
	PushChanges    ChangeDetectionStatus = "PushChanges"
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
