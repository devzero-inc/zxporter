/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	"go.uber.org/zap"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/devzero-inc/zxporter/internal/collector"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/transport"
	"github.com/devzero-inc/zxporter/internal/util"
)

// EnvBasedController is a controller that uses environment variables instead of CRDs
type EnvBasedController struct {
	client.Client
	Scheme            *runtime.Scheme
	Log               logr.Logger
	K8sClient         *kubernetes.Clientset
	DynamicClient     *dynamic.DynamicClient
	DiscoveryClient   *discovery.DiscoveryClient
	ApiExtensions     *apiextensionsclientset.Clientset
	Reconciler        *CollectionPolicyReconciler
	stopCh            chan struct{}
	reconcileInterval time.Duration
}

// NewEnvBasedController creates a new environment-based controller
func NewEnvBasedController(mgr ctrl.Manager, reconcileInterval time.Duration) (*EnvBasedController, error) {
	// Set up basic components
	logger := util.NewLogger("env-controller")
	zapLogger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("failed to create zap logger: %w", err)
	}

	// Create a Kubernetes clientset
	config := mgr.GetConfig()
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	kedaClientset, err := kedaclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create KEDA clientset: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client: %w", err)
	}

	apiExtensionClient, err := apiextensionsclientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create apiextensions client: %w", err)
	}

	// Create a shared Telemetry metrics instance
	sharedTelemetryMetrics := collector.NewTelemetryMetrics()

	reconciler := &CollectionPolicyReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Log:               logger.WithName("reconciler"),
		KEDAClient:        kedaClientset,
		K8sClient:         clientset,
		DynamicClient:     dynamicClient,
		DiscoveryClient:   discoveryClient,
		ApiExtensions:     apiExtensionClient,
		TelemetryMetrics:  sharedTelemetryMetrics,
		IsRunning:         false,
		RestartInProgress: false,
		ZapLogger:         zapLogger,
	}

	logger.Info("Checking 1st reconcile interval", "reconcile", reconcileInterval)

	// If no reconcile interval is specified, default to 5 minutes
	if reconcileInterval <= 0 {
		reconcileInterval = 5 * time.Minute
	}

	logger.Info("Checking 2nd reconcile interval", "reconcile", reconcileInterval)

	return &EnvBasedController{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Log:               logger,
		K8sClient:         clientset,
		DynamicClient:     dynamicClient,
		DiscoveryClient:   discoveryClient,
		ApiExtensions:     apiExtensionClient,
		Reconciler:        reconciler,
		stopCh:            make(chan struct{}),
		reconcileInterval: reconcileInterval,
	}, nil
}

// Start implements the Runnable interface for manager.Add
func (c *EnvBasedController) Start(ctx context.Context) error {
	c.Log.Info("Starting environment-based controller", "reconcileInterval", c.reconcileInterval)

	// Initialize Dakr sender and telemetry logger with context
	if err := c.initializeTelemetryComponents(ctx); err != nil {
		c.Log.Error(err, "Failed to initialize telemetry components")
		return fmt.Errorf("failed to initialize telemetry components: %w", err)
	}

	// Run the first reconciliation immediately
	if err := c.doReconcile(ctx); err != nil {
		c.Log.Error(err, "Failed initial reconciliation")
		// Continue running even if initial reconciliation fails
	}

	// Setup periodic reconciliation
	go c.runPeriodicReconciliation(ctx)

	// Wait for context cancellation
	<-ctx.Done()
	close(c.stopCh)
	c.Log.Info("Stopping environment-based controller")
	return nil
}

// NeedLeaderElection implements the LeaderElectionRunnable interface
func (c *EnvBasedController) NeedLeaderElection() bool {
	// This controller should only run on the leader
	return true
}

// runPeriodicReconciliation runs reconciliation periodically
func (c *EnvBasedController) runPeriodicReconciliation(ctx context.Context) {
	ticker := time.NewTicker(c.reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.doReconcile(ctx); err != nil {
				c.Log.Error(err, "Failed periodic reconciliation")
				// Continue running even if a reconciliation fails
			}
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// initializeTelemetryComponents initializes the Dakr sender and telemetry logger
func (c *EnvBasedController) initializeTelemetryComponents(ctx context.Context) error {
	// Load environment configuration to get Dakr URL and cluster token
	envSpec, err := util.LoadCollectionPolicySpecFromEnv()
	if err != nil {
		c.Log.Error(err, "Failed to load environment configuration for Dakr client setup")
		// Continue with default values
	}

	// Create dakr client and sender
	var dakrClient transport.DakrClient
	if envSpec.Policies.DakrURL != "" {
		dakrClient = transport.NewDakrClient(envSpec.Policies.DakrURL, envSpec.Policies.ClusterToken, c.Log)
		c.Log.Info("Created Dakr client with configured URL", "url", envSpec.Policies.DakrURL)
	} else {
		dakrClient = transport.NewSimpleDakrClient(c.Log)
		c.Log.Info("Created simple (logging) Dakr client because no URL was configured")
	}

	sender := transport.NewDirectSender(dakrClient, c.Log)

	// Initialize telemetry logger
	telemetryConfig := telemetry_logger.Config{
		BatchSize:     20,
		FlushInterval: 10 * time.Second,
		SendTimeout:   5 * time.Second,
		QueueSize:     100,
	}

	telemetryLogger := telemetry_logger.NewLogger(
		ctx,
		sender,
		telemetryConfig,
		c.Reconciler.ZapLogger,
		c.Log,
	)

	c.Reconciler.DakrClient = dakrClient
	c.Reconciler.Sender = sender
	c.Reconciler.TelemetryLogger = telemetryLogger

	c.Log.Info("Successfully initialized telemetry components")
	return nil
}

// doReconcile performs a single reconciliation
func (c *EnvBasedController) doReconcile(ctx context.Context) error {
	// c.Log.Info("Performing reconciliation based on environment variables")

	// Create a dummy request
	req := ctrl.Request{}

	// Trigger reconciliation
	_, err := c.Reconciler.Reconcile(ctx, req)
	if err != nil {
		return fmt.Errorf("reconciliation failed: %w", err)
	}

	// c.Log.Info("Reconciliation completed successfully")
	return nil
}
