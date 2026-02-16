package health

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComponentNames_AreDistinct(t *testing.T) {
	names := []string{
		ComponentCollectorManager,
		ComponentBufferQueue,
		ComponentDakrTransport,
		ComponentMpaServer,
		ComponentPrometheus,
	}
	seen := make(map[string]bool)
	for _, name := range names {
		assert.NotEmpty(t, name)
		assert.False(t, seen[name], "duplicate component name: %s", name)
		seen[name] = true
	}
}
