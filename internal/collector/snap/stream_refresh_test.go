package snap

import (
	"testing"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// TestMissingResourceHandlerKey_InSyncWithHandlers guards against silent drift
// between missingResourceHandlerKey (the wire ResourceType -> refresh-handler-key
// mapping used by the batched snapshot path) and the actual clusterHandlers /
// namespacedHandlers registrations.
//
// The batched missing-resource refresh (refreshMissingList) relies on this switch
// to route a missing resource back to the collector that can re-fetch it. Because
// the switch is maintained separately from the handler maps, a future handler
// added to initializeResourceHandlers / initializeNamespacedResourceHandlers could
// be silently unreachable — missing resources of that type would never be
// refreshed (they'd only converge via their regular collectors). This test makes
// that drift a build failure instead of a silent degradation.
func TestMissingResourceHandlerKey_InSyncWithHandlers(t *testing.T) {
	c := &ClusterSnapshotter{}
	// Building the handler maps only constructs closures; it does not touch any
	// client, so a zero-value snapshotter is sufficient.
	c.initializeResourceHandlers()
	c.initializeNamespacedResourceHandlers()

	type ref struct {
		key        string
		namespaced bool
	}
	reachable := map[ref]bool{}

	// Forward: every (key, namespaced) the switch can return must resolve to a
	// real handler, otherwise refresh fails at runtime with "handler not found".
	for v := range gen.ResourceType_name {
		key, namespaced, ok := missingResourceHandlerKey(gen.ResourceType(v))
		if !ok {
			continue
		}
		rt := gen.ResourceType(v)
		reachable[ref{key, namespaced}] = true

		if namespaced {
			if _, exists := c.namespacedHandlers[key]; !exists {
				t.Errorf("missingResourceHandlerKey(%s) returns namespaced key %q, but no namespacedHandlers entry exists", rt, key)
			}
		} else {
			if _, exists := c.clusterHandlers[key]; !exists {
				t.Errorf("missingResourceHandlerKey(%s) returns cluster key %q, but no clusterHandlers entry exists", rt, key)
			}
		}
	}

	// Reverse: every registered handler must be reachable from some ResourceType,
	// otherwise missing resources of that type are silently skipped in the batched
	// path (the drift this test exists to catch).
	for key := range c.namespacedHandlers {
		if !reachable[ref{key, true}] {
			t.Errorf("namespacedHandlers[%q] is not reachable from missingResourceHandlerKey; "+
				"missing resources of this type would be silently skipped in the batched snapshot refresh", key)
		}
	}
	for key := range c.clusterHandlers {
		if !reachable[ref{key, false}] {
			t.Errorf("clusterHandlers[%q] is not reachable from missingResourceHandlerKey; "+
				"missing resources of this type would be silently skipped in the batched snapshot refresh", key)
		}
	}
}
