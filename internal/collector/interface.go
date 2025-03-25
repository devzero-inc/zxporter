// internal/collector/interface.go
package collector

import (
	"context"
	"time"
)

// CollectedResource represents a resource collected from the Kubernetes API
type CollectedResource struct {
	// ResourceType is the type of resource (pod, container, node, etc.)
	ResourceType string

	// Object is the actual Kubernetes resource object
	Object interface{}

	// Timestamp is when the resource was collected
	Timestamp time.Time

	// EventType indicates whether this is an add, update, or delete event
	EventType string

	// Key is a unique identifier for this resource
	Key string
}

// ResourceCollector defines methods for collecting specific resource types
type ResourceCollector interface {
	// Start begins watching for resources
	Start(ctx context.Context) error

	// Stop halts watching for resources
	Stop() error

	// GetResourceChannel returns a channel for receiving collected resources
	GetResourceChannel() <-chan CollectedResource

	// GetType returns the type of resource this collector handles
	GetType() string
}
