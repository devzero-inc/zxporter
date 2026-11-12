package controller

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

	monitoringv1 "github.com/devzero-inc/zxporter/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
)

func TestCreateNewConfig_EnvVarCollection(t *testing.T) {
	cases := []struct {
		value        string
		wantDisabled bool
	}{
		{value: "", wantDisabled: false},
		{value: "enabled", wantDisabled: false},
		{value: "disabled", wantDisabled: true},
		// Unreachable through the loaders, which reject it; if it ever got
		// here it must withhold env vars rather than send them.
		{value: "bogus", wantDisabled: true},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			r := &CollectionPolicyReconciler{}
			cfg, _ := r.createNewConfig(&monitoringv1.CollectionPolicySpec{
				Policies: monitoringv1.Policies{EnvVarCollection: tc.value},
			}, logr.Discard())
			assert.Equal(t, tc.wantDisabled, cfg.DisableEnvVarCollection)
		})
	}
}

// Switching env var collection must resend every object, so it is a full
// restart (nil), not a selective one.
func TestIdentifyAffectedCollectors_FullRestartWhenEnvVarCollectionChanges(t *testing.T) {
	r := &CollectionPolicyReconciler{}
	for _, tc := range []struct {
		name     string
		old, new bool
	}{
		{name: "enabled to disabled", old: false, new: true},
		{name: "disabled to enabled", old: true, new: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r.identifyAffectedCollectors(
				&PolicyConfig{DisableEnvVarCollection: tc.old},
				&PolicyConfig{DisableEnvVarCollection: tc.new},
			)
			assert.Nil(t, got)
		})
	}

	unchanged := r.identifyAffectedCollectors(
		&PolicyConfig{DisableEnvVarCollection: true},
		&PolicyConfig{DisableEnvVarCollection: true},
	)
	assert.NotNil(t, unchanged, "an unchanged setting must not force a restart")
}

func TestMarkEnvVarStripping(t *testing.T) {
	in := []collector.CollectedResource{
		{ResourceType: collector.Pod, Key: "a"},
		{ResourceType: collector.Deployment, Key: "b"},
	}

	r := &CollectionPolicyReconciler{}
	assert.Equal(t, in, r.markEnvVarStripping(in), "enabled: resources go out as collected")

	r.stripEnvVars.Store(true)
	out := r.markEnvVarStripping(in)
	assert.Len(t, out, 2)
	for i := range out {
		assert.True(t, out[i].StripEnvVars, "disabled: every resource is marked")
		assert.Equal(t, in[i].Key, out[i].Key)
		// The collector's slice is read again for the MPA broadcast.
		assert.False(t, in[i].StripEnvVars, "the input slice must not be written")
	}
}
