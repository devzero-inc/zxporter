// internal/collector/event_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// EventCollector watches for event events and collects event data
type EventCollector struct {
	client           kubernetes.Interface
	informerFactory  informers.SharedInformerFactory
	eventInformer    cache.SharedIndexInformer
	resourceChan     chan CollectedResource
	stopCh           chan struct{}
	namespaces       []string
	excludedEvents   map[types.NamespacedName]bool
	maxEventsPerType int            // Limit events per type to prevent overwhelming the channel
	eventCounts      map[string]int // Track number of events per type
	retentionPeriod  time.Duration  // How long to keep events in memory
	logger           logr.Logger
	mu               sync.RWMutex
}

// ExcludedEvent defines an event to exclude from collection
type ExcludedEvent struct {
	Namespace string
	Name      string
}

// NewEventCollector creates a new collector for event resources
func NewEventCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedEvents []ExcludedEvent,
	maxEventsPerType int,
	retentionPeriod time.Duration,
	logger logr.Logger,
) *EventCollector {
	// Convert excluded events to a map for quicker lookups
	excludedEventsMap := make(map[types.NamespacedName]bool)
	for _, event := range excludedEvents {
		excludedEventsMap[types.NamespacedName{
			Namespace: event.Namespace,
			Name:      event.Name,
		}] = true
	}

	// Set default values if not specified
	if maxEventsPerType <= 0 {
		maxEventsPerType = 1000 // Default to 1000 events per type
	}

	if retentionPeriod <= 0 {
		retentionPeriod = 1 * time.Hour // Default to 1 hour retention
	}

	return &EventCollector{
		client:           client,
		resourceChan:     make(chan CollectedResource, 1000), // Events can be high volume
		stopCh:           make(chan struct{}),
		namespaces:       namespaces,
		excludedEvents:   excludedEventsMap,
		maxEventsPerType: maxEventsPerType,
		eventCounts:      make(map[string]int),
		retentionPeriod:  retentionPeriod,
		logger:           logger.WithName("event-collector"),
	}
}

// Start begins the event collection process
func (c *EventCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting event collector",
		"namespaces", c.namespaces,
		"maxEventsPerType", c.maxEventsPerType,
		"retentionPeriod", c.retentionPeriod)

	// Create informer factory based on namespace configuration
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Watch a specific namespace
		c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
			c.client,
			0, // No resync period, rely on events
			informers.WithNamespace(c.namespaces[0]),
		)
	} else {
		// Watch all namespaces
		c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)
	}

	// Create event informer
	c.eventInformer = c.informerFactory.Core().V1().Events().Informer()

	// Add event handlers
	_, err := c.eventInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			event := obj.(*corev1.Event)
			c.handleEvent(event, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldEvent := oldObj.(*corev1.Event)
			newEvent := newObj.(*corev1.Event)

			// Only handle meaningful updates
			if c.eventChanged(oldEvent, newEvent) {
				c.handleEvent(newEvent, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			event := obj.(*corev1.Event)
			c.handleEvent(event, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.eventInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start a goroutine to clean up old events
	go c.periodicCleanup(ctx)

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleEvent processes event events
func (c *EventCollector) handleEvent(event *corev1.Event, eventType string) {
	if c.isExcluded(event) {
		return
	}

	// Generate a type key for counting/grouping
	typeKey := fmt.Sprintf("%s/%s/%s", event.InvolvedObject.Kind, event.Type, event.Reason)

	// Check if we've hit the limit for this event type
	c.mu.Lock()
	count := c.eventCounts[typeKey]
	if count >= c.maxEventsPerType && eventType == "add" {
		c.mu.Unlock()
		c.logger.V(5).Info("Skipping event due to per-type limit",
			"namespace", event.Namespace,
			"name", event.Name,
			"reason", event.Reason,
			"count", count,
			"limit", c.maxEventsPerType)
		return
	}
	c.eventCounts[typeKey]++
	c.mu.Unlock()

	c.logger.Info("Processing event",
		"namespace", event.Namespace,
		"name", event.Name,
		"reason", event.Reason,
		"eventType", eventType)

	// Send the raw event object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: Event,
		Object:       event, // Send the entire event object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", event.Namespace, event.Name),
	}
}

// eventChanged detects meaningful changes in an event
func (c *EventCollector) eventChanged(oldEvent, newEvent *corev1.Event) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldEvent.ResourceVersion == newEvent.ResourceVersion {
		return false
	}

	// Check for count changes
	if oldEvent.Count != newEvent.Count {
		return true
	}

	// Check for last timestamp changes
	if !oldEvent.LastTimestamp.Equal(&newEvent.LastTimestamp) {
		return true
	}

	// Check for message changes
	if oldEvent.Message != newEvent.Message {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if an event should be excluded from collection
func (c *EventCollector) isExcluded(event *corev1.Event) bool {
	// Check if monitoring specific namespaces and this event isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == event.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if event is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: event.Namespace,
		Name:      event.Name,
	}
	return c.excludedEvents[key]
}

// periodicCleanup runs periodically to reset event counters for rate limiting
func (c *EventCollector) periodicCleanup(ctx context.Context) {
	ticker := time.NewTicker(c.retentionPeriod / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			// Reset all event counts
			c.eventCounts = make(map[string]int)
			c.mu.Unlock()

			c.logger.Info("Reset event rate limiting counters")
		}
	}
}

// Stop gracefully shuts down the event collector
func (c *EventCollector) Stop() error {
	c.logger.Info("Stopping event collector")
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *EventCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *EventCollector) GetType() string {
	return "event"
}
