package controller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPolicyConfig_FormattingRedactsClusterToken(t *testing.T) {
	cfg := &PolicyConfig{DakrURL: "https://dakr.example", ClusterToken: "dakr-secret-token"}

	for _, format := range []string{"%v", "%+v", "%s"} {
		for name, v := range map[string]any{"pointer": cfg, "value": *cfg} {
			got := fmt.Sprintf(format, v)
			assert.NotContains(t, got, "dakr-secret-token", "%s %s", format, name)
			assert.Contains(t, got, "ClusterToken:[REDACTED]", "%s %s", format, name)
			assert.Contains(t, got, "https://dakr.example", "%s %s", format, name)
		}
	}
	assert.Equal(t, "dakr-secret-token", cfg.ClusterToken, "formatting must not mutate the config")
	assert.Contains(t, fmt.Sprintf("%+v", PolicyConfig{}), "ClusterToken: ", "an empty token stays empty")
}
