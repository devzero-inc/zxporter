package collector

import (
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestPolicyCollectors_StopIsSafeAgainstInFlightEvents covers the
// send-after-close shutdown race for the five policy-domain collectors: an
// informer callback that fires while (or after) Stop closes batchChan must
// drop the event instead of panicking on a closed channel. Run with -race.
func TestPolicyCollectors_StopIsSafeAgainstInFlightEvents(t *testing.T) {
	cases := []struct {
		name string
		// build returns the collector's Stop, a fire func that drives one
		// event through the collector's informer-callback path, and the
		// batch channel (drained like the batcher does in production).
		build func() (stop func() error, fire func(), batchChan chan CollectedResource)
	}{
		{
			name: "kyverno policy",
			build: func() (func() error, func(), chan CollectedResource) {
				c := &KyvernoPolicyCollector{
					batchChan: make(chan CollectedResource, 1),
					stopCh:    make(chan struct{}),
					logger:    logr.Discard(),
				}
				return c.Stop, func() { c.handlePolicyEvent(clusterPolicyFixture(), EventTypeUpdate) }, c.batchChan
			},
		},
		{
			name: "kyverno policy report",
			build: func() (func() error, func(), chan CollectedResource) {
				c := &KyvernoPolicyReportCollector{
					batchChan:   make(chan CollectedResource, 1),
					stopCh:      make(chan struct{}),
					logger:      logr.Discard(),
					lastEmitted: make(map[string]uint64),
				}
				// Fire adds (not updates) so the no-op dedup never suppresses the send.
				return c.Stop, func() { c.handleReportEvent(policyReportFixture(1), EventTypeAdd) }, c.batchChan
			},
		},
		{
			name: "gatekeeper constraint template",
			build: func() (func() error, func(), chan CollectedResource) {
				c := &GatekeeperConstraintTemplateCollector{
					batchChan: make(chan CollectedResource, 1),
					stopCh:    make(chan struct{}),
					logger:    logr.Discard(),
				}
				return c.Stop, func() { c.handleTemplateEvent(constraintTemplateFixture(), EventTypeUpdate) }, c.batchChan
			},
		},
		{
			name: "gatekeeper constraint",
			build: func() (func() error, func(), chan CollectedResource) {
				c := &GatekeeperConstraintCollector{
					batchChan:   make(chan CollectedResource, 1),
					stopCh:      make(chan struct{}),
					logger:      logr.Discard(),
					watchedGVRs: make(map[schema.GroupVersionResource]bool),
				}
				return c.Stop, func() { c.handleConstraintEvent(constraintFixture(1), EventTypeUpdate) }, c.batchChan
			},
		},
		{
			name: "mig-parted config",
			build: func() (func() error, func(), chan CollectedResource) {
				c := &MigPartedConfigCollector{
					batchChan: make(chan CollectedResource, 1),
					stopCh:    make(chan struct{}),
					logger:    logr.Discard(),
				}
				return c.Stop, func() { c.handleConfigMapEvent(migPartedConfigMapFixture(), EventTypeUpdate) }, c.batchChan
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+" event after Stop is dropped", func(t *testing.T) {
			stop, fire, _ := tc.build()
			require.NoError(t, stop())
			assert.NotPanics(t, fire, "an event arriving after Stop must be dropped, not panic")
			assert.NotPanics(t, func() { require.NoError(t, stop()) }, "Stop must be idempotent")
		})

		t.Run(tc.name+" concurrent events during Stop", func(t *testing.T) {
			stop, fire, batchChan := tc.build()

			// Drain like the production batcher: consume until Stop closes the
			// channel. Without a drainer a full buffer would block senders
			// holding the read lock and turn this test into a deadlock rather
			// than exercising the race.
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				for range batchChan { //nolint:revive
				}
			}()

			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					for j := 0; j < 20; j++ {
						fire()
					}
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				assert.NoError(t, stop())
			}()
			close(start)
			wg.Wait()
			<-drained
		})
	}
}
