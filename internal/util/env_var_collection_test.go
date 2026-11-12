package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCollectionPolicySpecFromEnv_EnvVarCollection(t *testing.T) {
	cases := []struct {
		env     string
		want    string
		wantErr bool
	}{
		{env: "", want: ""},
		{env: "enabled", want: "enabled"},
		{env: " Disabled ", want: "disabled"},
		// A typo must fail loudly and still reach the controller, which reads
		// an unrecognised value as disabled, not as the default.
		{env: "true", want: "true", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(_ENV_ENV_VAR_COLLECTION, tc.env)
			spec, err := LoadCollectionPolicySpecFromEnv()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, spec.Policies.EnvVarCollection)
		})
	}
}

// Reconcile applies whatever spec comes back with a load error, so the
// opt-out must survive an unrelated invalid setting, and an invalid opt-out
// must not drop the settings loaded after it.
func TestLoadCollectionPolicySpecFromEnv_EnvVarCollectionSurvivesOtherErrors(t *testing.T) {
	t.Run("opt-out kept when an earlier setting is invalid", func(t *testing.T) {
		t.Setenv(_ENV_ENV_VAR_COLLECTION, "disabled")
		t.Setenv(_ENV_BUFFER_SIZE, "bad")
		spec, err := LoadCollectionPolicySpecFromEnv()
		require.Error(t, err)
		assert.Equal(t, "disabled", spec.Policies.EnvVarCollection)
	})
	t.Run("invalid opt-out keeps later settings", func(t *testing.T) {
		t.Setenv(_ENV_ENV_VAR_COLLECTION, "off")
		t.Setenv(_ENV_DISABLED_COLLECTORS, "pod")
		spec, err := LoadCollectionPolicySpecFromEnv()
		require.Error(t, err)
		assert.Equal(t, "off", spec.Policies.EnvVarCollection)
		assert.Equal(t, []string{"pod"}, spec.Policies.DisabledCollectors)
	})
}
