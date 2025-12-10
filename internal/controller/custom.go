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
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"github.com/devzero-inc/zxporter/internal/version"
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
	// Log version information at startup
	versionInfo := version.Get()

	c.Log.Info(
		"\n" +
			"\n" +
			"====================== ZXPORTER OPERATOR STARTING ======================\n" +
			fmt.Sprintf(" %-20s : %s\n", "Version", versionInfo.String()) +
			fmt.Sprintf(" %-20s : %s\n", "Git Commit", versionInfo.GitCommit) +
			fmt.Sprintf(" %-20s : %s\n", "Git Tree State", versionInfo.GitTreeState) +
			fmt.Sprintf(" %-20s : %s\n", "Build Date", versionInfo.BuildDate) +
			fmt.Sprintf(" %-20s : %s\n", "Go Version", versionInfo.GoVersion) +
			fmt.Sprintf(" %-20s : %s\n", "Compiler", versionInfo.Compiler) +
			fmt.Sprintf(" %-20s : %s\n", "Platform", versionInfo.Platform) +
			fmt.Sprintf(" %-20s : %s\n", "Reconcile Interval", c.reconcileInterval.String()) +
			"=======================================================================\n",
	)

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

	// Handle PAT token exchange if no cluster token is available
	if envSpec.Policies.ClusterToken == "" && envSpec.Policies.PATToken != "" {
		c.Log.Info("No cluster token found, attempting PAT token exchange")

		// Get cluster name and provider
		clusterName := envSpec.Policies.KubeContextName
		if clusterName == "" {
			clusterName = "zxporter-cluster"
		}
		
		k8sProvider := "other"
		if provider := os.Getenv("K8S_PROVIDER"); provider != "" {
			k8sProvider = provider
		}

		// Use a temporary DakrClient just for PAT exchange
		dakrURL := envSpec.Policies.DakrURL
		if dakrURL == "" {
			dakrURL = "https://dakr.devzero.io"
		}
		
		// Create a temporary client with empty cluster token for PAT exchange
		tempClient := transport.NewDakrClient(dakrURL, "", c.Log)
		
		// Exchange PAT for cluster token
		token, clusterId, err := tempClient.ExchangePATForClusterToken(ctx, envSpec.Policies.PATToken, clusterName, k8sProvider)
		if err != nil {
			c.Log.Error(err, "Failed to exchange PAT for cluster token")
		} else {
			c.Log.Info("Successfully obtained cluster token", "clusterId", clusterId)
			envSpec.Policies.ClusterToken = token

			// Persist the token to ConfigMap or Secret based on configuration
			if err := c.persistClusterToken(ctx, token); err != nil {
				c.Log.Error(err, "Failed to persist cluster token")
				// Continue anyway - the token is in memory
			}
		}
	}

	// Create dakr client and sender
	var dakrClient transport.DakrClient
	if envSpec.Policies.DakrURL != "" && envSpec.Policies.ClusterToken != "" {
		dakrClient = transport.NewDakrClient(envSpec.Policies.DakrURL, envSpec.Policies.ClusterToken, c.Log)
		c.Log.Info("Created Dakr client with configured URL", "url", envSpec.Policies.DakrURL)
	} else {
		dakrClient = transport.NewSimpleDakrClient(c.Log)
		c.Log.Info("Created simple (logging) Dakr client because no URL or token was configured")
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

// shouldUseSecretStorage determines whether to store tokens in Secret vs ConfigMap
func (c *EnvBasedController) shouldUseSecretStorage() bool {
	useSecret := os.Getenv("USE_SECRET_FOR_TOKEN")
	if useSecret == "" {
		// Try to read from file mounted at /etc/zxporter/config/USE_SECRET_FOR_TOKEN
		if data, err := os.ReadFile("/etc/zxporter/config/USE_SECRET_FOR_TOKEN"); err == nil {
			useSecret = strings.TrimSpace(string(data))
		}
	}
	return strings.ToLower(useSecret) == "true"
}

// persistClusterToken persists the cluster token to ConfigMap or Secret based on configuration
func (c *EnvBasedController) persistClusterToken(ctx context.Context, token string) error {
	if c.shouldUseSecretStorage() {
		return c.persistClusterTokenToSecret(ctx, token)
	}
	return c.persistClusterTokenToConfigMap(ctx, token)
}

// persistClusterTokenToConfigMap persists the cluster token to the ConfigMap
func (c *EnvBasedController) persistClusterTokenToConfigMap(ctx context.Context, token string) error {
	// Get namespace from environment variable or use default
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		// Try to read from service account namespace file
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			namespace = strings.TrimSpace(string(data))
		} else {
			// Fall back to default if all else fails
			namespace = "devzero-zxporter"
			c.Log.Info("Could not determine namespace, using default", "namespace", namespace)
		}
	}
	// Get ConfigMap name from environment variable with fallback to default
	configMapName := os.Getenv("TOKEN_CONFIGMAP_NAME")
	if configMapName == "" {
		// Try to read from file mounted at /etc/zxporter/config/TOKEN_CONFIGMAP_NAME
		if data, err := os.ReadFile("/etc/zxporter/config/TOKEN_CONFIGMAP_NAME"); err == nil {
			configMapName = strings.TrimSpace(string(data))
		}
		if configMapName == "" {
			// Fallback to default for backward compatibility
			configMapName = "devzero-zxporter-env-config"
		}
	}

	// Get the existing ConfigMap
	configMap, err := c.K8sClient.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	// Update the CLUSTER_TOKEN in the ConfigMap data
	if configMap.Data == nil {
		configMap.Data = make(map[string]string)
	}
	configMap.Data["CLUSTER_TOKEN"] = token

	// Update the ConfigMap
	_, err = c.K8sClient.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ConfigMap with cluster token: %w", err)
	}

	c.Log.Info("Successfully persisted cluster token to ConfigMap", "configMap", configMapName)
	return nil
}

// persistClusterTokenToSecret persists the cluster token to a Kubernetes Secret
func (c *EnvBasedController) persistClusterTokenToSecret(ctx context.Context, token string) error {
	// Get namespace from environment variable or use default
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		// Try to read from service account namespace file
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			namespace = strings.TrimSpace(string(data))
		} else {
			// Fall back to default if all else fails
			namespace = "devzero-zxporter"
			c.Log.Info("Could not determine namespace, using default", "namespace", namespace)
		}
	}
	
	// Get runtime Secret name from environment variable with fallback to default
	// This is the Secret where exchanged tokens are stored (system-managed)
	runtimeSecretName := os.Getenv("TOKEN_RUNTIME_SECRET_NAME")
	if runtimeSecretName == "" {
		// Try to read from file mounted at /etc/zxporter/config/TOKEN_RUNTIME_SECRET_NAME
		if data, err := os.ReadFile("/etc/zxporter/config/TOKEN_RUNTIME_SECRET_NAME"); err == nil {
			runtimeSecretName = strings.TrimSpace(string(data))
		}
		if runtimeSecretName == "" {
			// Fallback to TOKEN_SECRET_NAME for backward compatibility
			runtimeSecretName = os.Getenv("TOKEN_SECRET_NAME")
			if runtimeSecretName == "" {
				if data, err := os.ReadFile("/etc/zxporter/config/TOKEN_SECRET_NAME"); err == nil {
					runtimeSecretName = strings.TrimSpace(string(data))
				}
				if runtimeSecretName == "" {
					// Final fallback to default
					runtimeSecretName = "devzero-zxporter-token"
				}
			}
		}
	}

	// Try to get the existing Secret first
	secret, err := c.K8sClient.CoreV1().Secrets(namespace).Get(ctx, runtimeSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Create new Secret if it doesn't exist
			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      runtimeSecretName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/name": "zxporter",
						"app.kubernetes.io/component": "token-storage",
					},
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					"CLUSTER_TOKEN": []byte(token),
				},
			}
			_, err = c.K8sClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create Secret: %w", err)
			}
			c.Log.Info("Successfully created Secret with cluster token", "secret", runtimeSecretName)
		} else {
			return fmt.Errorf("failed to get Secret: %w", err)
		}
	} else {
		// Update existing Secret
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data["CLUSTER_TOKEN"] = []byte(token)
		
		_, err = c.K8sClient.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update Secret with cluster token: %w", err)
		}
		c.Log.Info("Successfully updated Secret with cluster token", "secret", runtimeSecretName)
	}

	return nil
}
