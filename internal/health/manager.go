package health

import (
	"fmt"
	"sync"
	"time"
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
	mu                  sync.RWMutex
	components          map[string]*ComponentStatus
	livenessGraceUntil  time.Time // LivenessCheck always passes before this deadline
	readinessGraceUntil time.Time // ReadinessCheck always passes before this deadline
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
			m := make(map[string]string, len(metadata))
			for k, v := range metadata {
				m[k] = v
			}
			comp.Metadata = m
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
		meta := make(map[string]string, len(comp.Metadata))
		for k, v := range comp.Metadata {
			meta[k] = v
		}
		report[name] = ComponentStatus{
			Status:   comp.Status,
			Message:  comp.Message,
			Metadata: meta,
		}
	}
	return report
}

// SuppressLiveness makes LivenessCheck pass unconditionally for the given
// duration. Use this before a planned collector restart so that the transient
// Unhealthy window does not trigger a pod kill. The grace period is cleared
// automatically when StartAll succeeds (via ClearLivenessSuppression) or when
// the deadline expires.
func (hm *HealthManager) SuppressLiveness(d time.Duration) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	hm.livenessGraceUntil = time.Now().Add(d)
}

// ClearLivenessSuppression removes any active grace period so LivenessCheck
// resumes normal evaluation. Call this after collectors are back up.
func (hm *HealthManager) ClearLivenessSuppression() {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	hm.livenessGraceUntil = time.Time{}
}

// SuppressReadiness makes ReadinessCheck pass unconditionally for the given
// duration. Use this at startup so the pod can become ready while waiting for
// leader election. The grace period is cleared automatically when collectors
// start (via ClearReadinessSuppression) or when the deadline expires.
func (hm *HealthManager) SuppressReadiness(d time.Duration) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	hm.readinessGraceUntil = time.Now().Add(d)
}

// ClearReadinessSuppression removes any active readiness grace period so
// ReadinessCheck resumes normal evaluation.
func (hm *HealthManager) ClearReadinessSuppression() {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	hm.readinessGraceUntil = time.Time{}
}

// LivenessCheck checks if all components are at least Degraded (not Unhealthy).
// During an active grace period (set via SuppressLiveness) it always returns nil
// so that planned restarts do not trigger pod kills.
func (hm *HealthManager) LivenessCheck() error {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	return hm.livenessCheckLocked()
}

// livenessCheckLocked performs the liveness check while the caller holds mu.
func (hm *HealthManager) livenessCheckLocked() error {
	if !hm.livenessGraceUntil.IsZero() && time.Now().Before(hm.livenessGraceUntil) {
		return nil
	}

	component, exists := hm.components[ComponentCollectorManager]
	if exists && component.Status == HealthStatusUnhealthy {
		return fmt.Errorf("%s is %s: %s", ComponentCollectorManager, component.Status, component.Message)
	}

	return nil
}

// ReadinessCheck checks if all required components are Healthy or Degraded.
func (hm *HealthManager) ReadinessCheck() error {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	return hm.readinessCheckLocked()
}

// readinessCheckLocked performs the readiness check while the caller holds mu.
func (hm *HealthManager) readinessCheckLocked() error {
	if !hm.readinessGraceUntil.IsZero() && time.Now().Before(hm.readinessGraceUntil) {
		return nil
	}

	readyComponents := []string{ComponentCollectorManager, ComponentDakrTransport}
	for _, compName := range readyComponents {
		component, exists := hm.components[compName]
		if !exists {
			return fmt.Errorf("%s is not registered", compName)
		}
		if component.Status != HealthStatusHealthy && component.Status != HealthStatusDegraded {
			return fmt.Errorf("%s is not ready (status: %s)", compName, component.Status)
		}
	}
	return nil
}

// CheckLiveness returns the report and liveness error atomically under a single
// lock acquisition, avoiding TOCTOU between BuildReport and LivenessCheck.
func (hm *HealthManager) CheckLiveness() (map[string]ComponentStatus, error) {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	return hm.buildReportLocked(), hm.livenessCheckLocked()
}

// CheckReadiness returns the report and readiness error atomically.
func (hm *HealthManager) CheckReadiness() (map[string]ComponentStatus, error) {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	return hm.buildReportLocked(), hm.readinessCheckLocked()
}

// buildReportLocked builds the report while the caller holds mu.
func (hm *HealthManager) buildReportLocked() map[string]ComponentStatus {
	report := make(map[string]ComponentStatus, len(hm.components))
	for name, comp := range hm.components {
		meta := make(map[string]string, len(comp.Metadata))
		for k, v := range comp.Metadata {
			meta[k] = v
		}
		report[name] = ComponentStatus{
			Status:   comp.Status,
			Message:  comp.Message,
			Metadata: meta,
		}
	}
	return report
}

// String returns a human-readable representation of the HealthStatus
func (s HealthStatus) String() string {
	switch s {
	case HealthStatusHealthy:
		return "healthy"
	case HealthStatusDegraded:
		return "degraded"
	case HealthStatusUnhealthy:
		return "unhealthy"
	default:
		return "unspecified"
	}
}
