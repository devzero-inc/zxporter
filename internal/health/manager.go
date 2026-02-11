package health

import (
	"sync"
)

// HealthStatus matches proto enum for easy mapping
type HealthStatus int

// HealthStatus values
const (
	HealthStatusUnspecified HealthStatus = iota
	HealthStatusHealthy
	HealthStatusDegraded
	HealthStatusUnhealthy
)

// ComponentStatus holds the health status, message, and metadata for a component
type ComponentStatus struct {
	Status   HealthStatus
	Message  string
	Metadata map[string]string
}

type HealthManager struct {
	mu         sync.RWMutex
	components map[string]*ComponentStatus
}

// NewHealthManager creates a new HealthManager
func NewHealthManager() *HealthManager {
	return &HealthManager{
		components: make(map[string]*ComponentStatus),
	}
}

// Register adds a component to the health registry
func (hm *HealthManager) Register(name string) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	if _, exists := hm.components[name]; !exists {
		hm.components[name] = &ComponentStatus{
			Status:   HealthStatusUnspecified,
			Message:  "",
			Metadata: make(map[string]string),
		}
	}
}

// Deregister removes a component from the health registry
func (hm *HealthManager) Deregister(name string) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	delete(hm.components, name)
}

// UpdateStatus updates the health status, message, and metadata for a component
func (hm *HealthManager) UpdateStatus(name string, status HealthStatus, message string, metadata map[string]string) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	if comp, exists := hm.components[name]; exists {
		comp.Status = status
		comp.Message = message
		if metadata != nil {
			comp.Metadata = metadata
		}
	}
}

// GetStatus retrieves the current status for a component
func (hm *HealthManager) GetStatus(name string) (ComponentStatus, bool) {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	comp, exists := hm.components[name]
	if !exists {
		return ComponentStatus{}, false
	}
	meta := make(map[string]string, len(comp.Metadata))
	for k, v := range comp.Metadata {
		meta[k] = v
	}
	return ComponentStatus{
		Status:   comp.Status,
		Message:  comp.Message,
		Metadata: meta,
	}, true
}

// BuildReport returns a snapshot of all component statuses
func (hm *HealthManager) BuildReport() map[string]ComponentStatus {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	report := make(map[string]ComponentStatus, len(hm.components))
	for name, comp := range hm.components {
		report[name] = *comp
	}
	return report
}
