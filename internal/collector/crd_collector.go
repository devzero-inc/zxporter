package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

type CRDCollector struct {
	client          apiextclientset.Interface
	informer        cache.SharedIndexInformer
	batchChan       chan CollectedResource
	resourceChan    chan []CollectedResource
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger

	// mu guards stopped and, with it, the lifetime of batchChan: senders hold
	// the read lock, Stop takes the write lock before closing the channel.
	mu      sync.RWMutex
	stopped bool

	// stopOnce makes Stop idempotent. StopAll cancels the collector context —
	// waking the watcher goroutine below, which calls Stop — and then calls Stop
	// itself, so two Stops genuinely race.
	stopOnce sync.Once
}

func NewCRDCollector(
	client apiextclientset.Interface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *CRDCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &CRDCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("crd-collector"),
		telemetryLogger: telemetryLogger,
	}
}

func (c *CRDCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting CRD collector")

	// A stopped collector's channels are already closed, so restarting this
	// instance would double-close them. Callers build a fresh collector instead.
	c.mu.RLock()
	stopped := c.stopped
	c.mu.RUnlock()
	if stopped {
		return fmt.Errorf("CRD collector has been stopped and cannot be restarted")
	}

	factory := apiextinformers.NewSharedInformerFactoryWithOptions(
		c.client,
		0,
		apiextinformers.WithTransform(StripMetadataTransform),
	)
	c.informer = factory.Apiextensions().V1().CustomResourceDefinitions().Informer()

	_, err := c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if crd, ok := obj.(*apiextv1.CustomResourceDefinition); ok {
				c.handleCRDEvent(crd, EventTypeAdd)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if crd, ok := newObj.(*apiextv1.CustomResourceDefinition); ok {
				c.handleCRDEvent(crd, EventTypeUpdate)
			}
		},
		DeleteFunc: c.handleCRDDelete,
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// The batcher is the only consumer of batchChan, so it has to be running
	// before the informer can produce. The informer synthesises an Add for
	// every CRD that already exists, so batchChan takes one item per CRD in the
	// cluster as soon as the cache syncs; with no consumer yet, any cluster
	// bigger than batchChan's buffer would wedge this method forever.
	c.batcher.start()

	factory.Start(c.stopCh)
	if !cache.WaitForCacheSync(c.stopCh, c.informer.HasSynced) {
		// WaitForCacheSync only reports false when stopCh closes, so this is a
		// shutdown during startup, never a timeout. Stop owns the batcher's
		// lifetime, so leave it to finish draining.
		return fmt.Errorf("CRD collector stopped before its informer cache synced")
	}

	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-c.stopCh:
		}
	}()

	return nil
}

// handleCRDDelete forwards a CRD deletion to the batcher. A missed delete
// arrives as a tombstone wrapping the last known object — StripMetadataTransform
// passes those through untouched — so unwrap it rather than asserting blindly,
// which would panic on the informer's handler goroutine and kill the process.
func (c *CRDCollector) handleCRDDelete(obj interface{}) {
	crd, ok := obj.(*apiextv1.CustomResourceDefinition)
	if !ok {
		if tombstone, isTombstone := obj.(cache.DeletedFinalStateUnknown); isTombstone {
			crd, ok = tombstone.Obj.(*apiextv1.CustomResourceDefinition)
		}
	}
	if !ok {
		c.logger.Info("Ignoring CRD delete event of unexpected type", "type", fmt.Sprintf("%T", obj))
		return
	}
	c.handleCRDEvent(crd, EventTypeDelete)
}

// handleCRDEvent forwards a CRD event to the batcher. It runs on the informer's
// handler goroutine, so it must neither send on a closed batchChan nor stay
// parked on a full one once the collector is stopping.
func (c *CRDCollector) handleCRDEvent(crd *apiextv1.CustomResourceDefinition, eventType EventType) {
	resource := CollectedResource{
		ResourceType: CustomResourceDefinition,
		Object:       getCleanCRDJSON(crd, c.logger),
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          crd.Name,
	}

	// The read lock keeps Stop from closing batchChan underneath an in-flight
	// send; the stopCh arm releases a send that is blocked on a full buffer.
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.stopped {
		return
	}

	select {
	case c.batchChan <- resource:
	case <-c.stopCh:
	}
}

func (c *CRDCollector) Stop() error {
	c.logger.Info("Stopping CRD collector")

	// Concurrent callers block here until the first Stop has fully finished, so
	// every close below happens exactly once.
	c.stopOnce.Do(func() {
		// 1. Stop the informer factory and release any handler goroutine parked
		//    on a full batchChan, so step 2 cannot block behind one.
		close(c.stopCh)
		c.logger.Info("Closed CRD collector stop channel")

		// 2. Taking the write lock waits for every in-flight handleCRDEvent to
		//    leave its send, so closing batchChan cannot panic a live sender.
		//    Later events see stopped and return without sending.
		c.mu.Lock()
		c.stopped = true
		close(c.batchChan)
		c.mu.Unlock()
		c.logger.Info("Closed CRD collector batch input channel")

		// 3. The batcher drains the closed batchChan and closes resourceChan.
		if c.batcher != nil {
			c.batcher.stop()
			c.logger.Info("CRD collector batcher stopped")
		}
	})

	return nil
}

func (c *CRDCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

func (c *CRDCollector) GetType() string {
	return "crd"
}

func (c *CRDCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.client.ApiextensionsV1().
		CustomResourceDefinitions().
		List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// AddResource manually adds a CRD resource to be processed by the collector
func (c *CRDCollector) AddResource(resource interface{}) error {
	crd, ok := resource.(*apiextv1.CustomResourceDefinition)
	if !ok {
		return fmt.Errorf("expected *apiextensionsv1.CustomResourceDefinition, got %T", resource)
	}

	c.handleCRDEvent(crd, EventTypeAdd)
	return nil
}

func getCleanCRDJSON(crd *apiextv1.CustomResourceDefinition, logger logr.Logger) interface{} {
	raw, err := json.Marshal(crd)
	if err != nil {
		logger.Error(err, "Failed to marshal CRD")
		return crd
	}

	var clean map[string]interface{}
	if err := json.Unmarshal(raw, &clean); err != nil {
		logger.Error(err, "Failed to unmarshal CRD")
		return crd
	}
	return clean
}
