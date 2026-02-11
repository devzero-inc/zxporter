package health

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

const testCollectorManager = "collector_manager"

// Test cases for HealthManager
func TestRegisterAndUpdateStatus_CollectorManager(t *testing.T) {
	healthMgr := NewHealthManager()
	component := testCollectorManager
	metadata := map[string]string{"active": "55", "total": "60"}

	healthMgr.Register(component)
	healthMgr.UpdateStatus(component, HealthStatusHealthy, "55/60 collectors active", metadata)

	status, exists := healthMgr.GetStatus(component)
	assert.True(t, exists, "collector_manager should be registered")
	assert.Equal(t, HealthStatusHealthy, status.Status)
	assert.Equal(t, "55/60 collectors active", status.Message)
	assert.Equal(t, metadata, status.Metadata)
}

func TestUpdateStatus_Degraded(t *testing.T) {
	healthMgr := NewHealthManager()
	component := testCollectorManager
	metadata := map[string]string{"active": "30", "total": "60"}

	healthMgr.Register(component)
	healthMgr.UpdateStatus(component, HealthStatusDegraded, "30/60 collectors active", metadata)

	status, exists := healthMgr.GetStatus(component)
	assert.True(t, exists, "collector_manager should be registered")
	assert.Equal(t, HealthStatusDegraded, status.Status)
	assert.Equal(t, "30/60 collectors active", status.Message)
	assert.Equal(t, metadata, status.Metadata)
}

func TestUpdateStatus_Unhealthy(t *testing.T) {
	healthMgr := NewHealthManager()
	component := testCollectorManager
	metadata := map[string]string{"active": "0", "total": "60"}

	healthMgr.Register(component)
	healthMgr.UpdateStatus(component, HealthStatusUnhealthy, "No collectors running", metadata)

	status, exists := healthMgr.GetStatus(component)
	assert.True(t, exists, "collector_manager should be registered")
	assert.Equal(t, HealthStatusUnhealthy, status.Status)
	assert.Equal(t, "No collectors running", status.Message)
	assert.Equal(t, metadata, status.Metadata)
}

func TestConcurrentUpdatesAndReads(t *testing.T) {
	healthMgr := NewHealthManager()
	component := testCollectorManager
	healthMgr.Register(component)

	statuses := []HealthStatus{HealthStatusHealthy, HealthStatusDegraded, HealthStatusUnhealthy}
	messages := []string{"All good", "Some issues", "Down"}

	var wg sync.WaitGroup
	updateCount := 100

	// Start goroutines to update status
	for i := range updateCount {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status := statuses[i%len(statuses)]
			msg := messages[i%len(messages)]
			meta := map[string]string{"iteration": strconv.Itoa(i)}
			healthMgr.UpdateStatus(component, status, msg, meta)
		}(i)
	}

	// Start goroutines to read status
	for range updateCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = healthMgr.GetStatus(component)
			_ = healthMgr.BuildReport()
		}()
	}

	wg.Wait()

	// Final state should be one of the possible values
	status, exists := healthMgr.GetStatus(component)
	assert.True(t, exists, "collector_manager should be registered")
	assert.Contains(t, statuses, status.Status)
	assert.Contains(t, messages, status.Message)
}
