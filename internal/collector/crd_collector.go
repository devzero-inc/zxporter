package collector

import (
	"context"
	"fmt"
	"time"
	"sync"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"github.com/go-logr/logr"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	"k8s.io/client-go/tools/cache"
	apiextclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
)

type CRDCollector struct {
	client       apiextclientset.Interface
	informer     cache.SharedIndexInformer
	batchChan    chan CollectedResource
	resourceChan chan []CollectedResource
	batcher      *ResourcesBatcher
	stopCh       chan struct{}
	logger       logr.Logger
	mu           sync.RWMutex
}

func NewCRDCollector(
	client apiextclientset.Interface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *CRDCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &CRDCollector{
		client:       client,
		batchChan:    batchChan,
		resourceChan: resourceChan,
		batcher:      batcher,
		stopCh:       make(chan struct{}),
		logger:       logger.WithName("crd-collector"),
	}
}

func (c *CRDCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting CRD collector")

	factory := apiextinformers.NewSharedInformerFactory(c.client, 0)
	c.informer = factory.Apiextensions().V1().CustomResourceDefinitions().Informer()

	_, err := c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			crd := obj.(*apiextv1.CustomResourceDefinition)
			c.handleCRDEvent(crd, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			crd := newObj.(*apiextv1.CustomResourceDefinition)
			c.handleCRDEvent(crd, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			crd := obj.(*apiextv1.CustomResourceDefinition)
			c.handleCRDEvent(crd, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	factory.Start(c.stopCh)
	if !cache.WaitForCacheSync(c.stopCh, c.informer.HasSynced) {
		return fmt.Errorf("timed out waiting for CRD informer cache to sync")
	}

	existingCRDs := c.informer.GetStore().List()
	for _, obj := range existingCRDs {
		crd, ok := obj.(*apiextv1.CustomResourceDefinition)
		if !ok {
			c.logger.Error(nil, "Failed to cast object to CRD during initial emit")
			continue
		}
		c.handleCRDEvent(crd.DeepCopy(), EventTypeAdd)
	}

	c.batcher.start()

	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-c.stopCh:
		}
	}()

	return nil
}

func (c *CRDCollector) handleCRDEvent(crd *apiextv1.CustomResourceDefinition, eventType EventType) {
	c.logger.Info("Processing CRD event", "name", crd.Name, "eventType", eventType.String())

	c.batchChan <- CollectedResource{
		ResourceType: CustomResourceDefinition,
		Object:       getCleanCRDJSON(crd, c.logger),
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          crd.Name,
	}
}

func (c *CRDCollector) Stop() error {
	c.logger.Info("Stopping CRD collector")

	select {
	case <-c.stopCh:
		c.logger.Info("CRD collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed CRD collector stop channel")
	}

	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed CRD collector batch input channel")
	}

	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("CRD collector batcher stopped")
	}

	return nil
}

func (c *CRDCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

func (c *CRDCollector) GetType() string {
	return "crd"
}

func (c *CRDCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.client.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
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


