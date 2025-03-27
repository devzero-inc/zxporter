// internal/collector/secret_collector.go
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

// SecretCollector watches for secret events and collects secret data
type SecretCollector struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	secretInformer  cache.SharedIndexInformer
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	namespaces      []string
	excludedSecrets map[types.NamespacedName]bool
	logger          logr.Logger
	mu              sync.RWMutex
	// Configuration for handling sensitive data
	maskSecretData bool
}

// ExcludedSecret defines a secret to exclude from collection
type ExcludedSecret struct {
	Namespace string
	Name      string
}

// NewSecretCollector creates a new collector for secret resources
func NewSecretCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedSecrets []ExcludedSecret,
	maskSecretData bool,
	logger logr.Logger,
) *SecretCollector {
	// Convert excluded secrets to a map for quicker lookups
	excludedSecretsMap := make(map[types.NamespacedName]bool)
	for _, secret := range excludedSecrets {
		excludedSecretsMap[types.NamespacedName{
			Namespace: secret.Namespace,
			Name:      secret.Name,
		}] = true
	}

	return &SecretCollector{
		client:          client,
		resourceChan:    make(chan CollectedResource, 100),
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		excludedSecrets: excludedSecretsMap,
		maskSecretData:  maskSecretData,
		logger:          logger.WithName("secret-collector"),
	}
}

// Start begins the secret collection process
func (c *SecretCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting secret collector",
		"namespaces", c.namespaces,
		"maskSecretData", c.maskSecretData)

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

	// Create secret informer
	c.secretInformer = c.informerFactory.Core().V1().Secrets().Informer()

	// Add event handlers
	_, err := c.secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			secret := obj.(*corev1.Secret)
			c.handleSecretEvent(secret, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldSecret := oldObj.(*corev1.Secret)
			newSecret := newObj.(*corev1.Secret)

			// Only handle meaningful updates
			if c.secretChanged(oldSecret, newSecret) {
				c.handleSecretEvent(newSecret, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			secret := obj.(*corev1.Secret)
			c.handleSecretEvent(secret, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.secretInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleSecretEvent processes secret events
func (c *SecretCollector) handleSecretEvent(secret *corev1.Secret, eventType string) {
	if c.isExcluded(secret) {
		return
	}

	c.logger.V(4).Info("Processing secret event",
		"namespace", secret.Namespace,
		"name", secret.Name,
		"type", secret.Type,
		"eventType", eventType)

	// Create a safe copy of the secret before sending
	secretToSend := c.sanitizeSecret(secret)

	// Send the sanitized secret object to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: Secret,
		Object:       secretToSend,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", secret.Namespace, secret.Name),
	}
}

// sanitizeSecret creates a copy of the secret with sensitive data masked if configured
func (c *SecretCollector) sanitizeSecret(secret *corev1.Secret) *corev1.Secret {
	// Create a deep copy of the secret
	secretCopy := secret.DeepCopy()

	// If we're not masking secret data, return the copy as is
	if !c.maskSecretData {
		return secretCopy
	}

	// Otherwise, mask the actual data while preserving keys
	if secretCopy.Data != nil {
		for key := range secretCopy.Data {
			// Replace each value with [REDACTED] placeholder
			// Use a byte slice of "[REDACTED]" as the value
			secretCopy.Data[key] = []byte("[REDACTED]")
		}
	}

	if secretCopy.StringData != nil {
		for key := range secretCopy.StringData {
			secretCopy.StringData[key] = "[REDACTED]"
		}
	}

	return secretCopy
}

// secretChanged detects meaningful changes in a secret
func (c *SecretCollector) secretChanged(oldSecret, newSecret *corev1.Secret) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldSecret.ResourceVersion == newSecret.ResourceVersion {
		return false
	}

	// Check if type changed
	if oldSecret.Type != newSecret.Type {
		return true
	}

	// Check for changes in data keys (not values, just the presence of keys)
	if len(oldSecret.Data) != len(newSecret.Data) {
		return true
	}

	for key := range oldSecret.Data {
		if _, exists := newSecret.Data[key]; !exists {
			return true
		}
	}

	// For security reasons, we don't compare the actual data values
	// Even a content change is reported as a meaningful change
	// This ensures we capture all updates without comparing sensitive data
	if len(oldSecret.Data) > 0 || len(newSecret.Data) > 0 {
		return true
	}

	// Check for immutable flag changes
	if (oldSecret.Immutable == nil && newSecret.Immutable != nil) ||
		(oldSecret.Immutable != nil && newSecret.Immutable == nil) ||
		(oldSecret.Immutable != nil && newSecret.Immutable != nil && *oldSecret.Immutable != *newSecret.Immutable) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldSecret.Labels, newSecret.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldSecret.Annotations, newSecret.Annotations) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a secret should be excluded from collection
func (c *SecretCollector) isExcluded(secret *corev1.Secret) bool {
	// Check if monitoring specific namespaces and this secret isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == secret.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if secret is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: secret.Namespace,
		Name:      secret.Name,
	}
	return c.excludedSecrets[key]
}

// Stop gracefully shuts down the secret collector
func (c *SecretCollector) Stop() error {
	c.logger.Info("Stopping secret collector")
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *SecretCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *SecretCollector) GetType() string {
	return "secret"
}
