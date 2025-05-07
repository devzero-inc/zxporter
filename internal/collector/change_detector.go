package collector

import (
	"fmt"
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
		c.logger.V(3).Info(fmt.Sprintf("Ignoring %s update with same resource version", kind), kind, resourceName)
		return IgnoreChanges
	}

	// Check for label changes
	if !mapsEqual(old.Labels, new.Labels) {
		c.logger.V(3).Info(fmt.Sprintf("Pushing %s changes due to labels change", kind), kind, resourceName)
		return PushChanges
	}

	// Check for annotation changes
	if !mapsEqual(old.Annotations, new.Annotations) {
		c.logger.V(3).Info(fmt.Sprintf("Pushing %s changes due to annotations change", kind), kind, resourceName)
		return PushChanges
	}

	return UnknownChanges
}
