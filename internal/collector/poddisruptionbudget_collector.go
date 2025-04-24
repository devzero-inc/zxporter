// internal/collector/poddisruptionbudget_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// PodDisruptionBudgetCollector watches for PDB events and collects PDB data
type PodDisruptionBudgetCollector struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	pdbInformer     cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	namespaces      []string
	excludedPDBs    map[types.NamespacedName]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// ExcludedPDB defines a PodDisruptionBudget to exclude from collection
type ExcludedPDB struct {
	Namespace string
	Name      string
}

// NewPodDisruptionBudgetCollector creates a new collector for PDB resources
func NewPodDisruptionBudgetCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedPDBs []ExcludedPDB,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *PodDisruptionBudgetCollector {
	// Convert excluded PDBs to a map for quicker lookups
	excludedPDBsMap := make(map[types.NamespacedName]bool)
	for _, pdb := range excludedPDBs {
		excludedPDBsMap[types.NamespacedName{
			Namespace: pdb.Namespace,
			Name:      pdb.Name,
		}] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &PodDisruptionBudgetCollector{
		client:       client,
		batchChan:    batchChan,
		resourceChan: resourceChan,
		batcher:      batcher,
		stopCh:       make(chan struct{}),
		namespaces:   namespaces,
		excludedPDBs: excludedPDBsMap,
		logger:       logger.WithName("pdb-collector"),
	}
}

// Start begins the PDB collection process
func (c *PodDisruptionBudgetCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting PodDisruptionBudget collector", "namespaces", c.namespaces)

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

	// Create PDB informer
	c.pdbInformer = c.informerFactory.Policy().V1().PodDisruptionBudgets().Informer()

	// Add event handlers
	_, err := c.pdbInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pdb := obj.(*policyv1.PodDisruptionBudget)
			c.handlePDBEvent(pdb, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPDB := oldObj.(*policyv1.PodDisruptionBudget)
			newPDB := newObj.(*policyv1.PodDisruptionBudget)

			// Only handle meaningful updates
			if c.pdbChanged(oldPDB, newPDB) {
				c.handlePDBEvent(newPDB, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			pdb := obj.(*policyv1.PodDisruptionBudget)
			c.handlePDBEvent(pdb, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.pdbInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for PDBs")
	c.batcher.start()

	// Keep this goroutine alive until context cancellation or stop
	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-stopCh:
			// Channel was closed by Stop() method
		}
	}()

	return nil
}

// handlePDBEvent processes PDB events
func (c *PodDisruptionBudgetCollector) handlePDBEvent(pdb *policyv1.PodDisruptionBudget, eventType string) {
	if c.isExcluded(pdb) {
		return
	}

	c.logger.V(4).Info("Processing PodDisruptionBudget event",
		"namespace", pdb.Namespace,
		"name", pdb.Name,
		"eventType", eventType)

	// Send the raw PDB object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: PodDisruptionBudget,
		Object:       pdb, // Send the entire PDB object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", pdb.Namespace, pdb.Name),
	}
}

// pdbChanged detects meaningful changes in a PDB
func (c *PodDisruptionBudgetCollector) pdbChanged(oldPDB, newPDB *policyv1.PodDisruptionBudget) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldPDB.ResourceVersion == newPDB.ResourceVersion {
		return false
	}

	// Check for spec changes
	if !reflect.DeepEqual(oldPDB.Spec, newPDB.Spec) {
		return true
	}

	// Check for status changes
	if oldPDB.Status.DisruptionsAllowed != newPDB.Status.DisruptionsAllowed ||
		oldPDB.Status.CurrentHealthy != newPDB.Status.CurrentHealthy ||
		oldPDB.Status.DesiredHealthy != newPDB.Status.DesiredHealthy ||
		oldPDB.Status.ExpectedPods != newPDB.Status.ExpectedPods {
		return true
	}

	// Check for condition changes
	if !reflect.DeepEqual(oldPDB.Status.Conditions, newPDB.Status.Conditions) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldPDB.Labels, newPDB.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldPDB.Annotations, newPDB.Annotations) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a PDB should be excluded from collection
func (c *PodDisruptionBudgetCollector) isExcluded(pdb *policyv1.PodDisruptionBudget) bool {
	// Check if monitoring specific namespaces and this PDB isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == pdb.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if PDB is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: pdb.Namespace,
		Name:      pdb.Name,
	}
	return c.excludedPDBs[key]
}

// Stop gracefully shuts down the PDB collector
func (c *PodDisruptionBudgetCollector) Stop() error {
	c.logger.Info("Stopping PodDisruptionBudget collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("PDB collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed PDB collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed PDB collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("PDB collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *PodDisruptionBudgetCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *PodDisruptionBudgetCollector) GetType() string {
	return "pod_disruption_budget"
}

// IsAvailable checks if PodDisruptionBudget resources can be accessed in the cluster
func (c *PodDisruptionBudgetCollector) IsAvailable(ctx context.Context) bool {
	return true
}
