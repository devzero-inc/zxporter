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
	"net/http"
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

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/health"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/transport"
	"github.com/devzero-inc/zxporter/internal/util"
	"github.com/devzero-inc/zxporter/internal/version"
)

// EnvBasedController is a controller that uses environment variables instead of CRDs
type EnvBasedController struct {
	client.Client
	Scheme              *runtime.Scheme
	Log                 logr.Logger
	K8sClient           kubernetes.Interface
	DynamicClient       *dynamic.DynamicClient
	DiscoveryClient     *discovery.DiscoveryClient
	ApiExtensions       *apiextensionsclientset.Clientset
	Reconciler          *CollectionPolicyReconciler
	stopCh              chan struct{}
	reconcileInterval   time.Duration
	mpaServerPort       int
	startTime           time.Time
	nodeOperatorMonitor *health.NodeOperatorMonitor
	dakrClientFactory   func(dakrBaseURL string, clusterToken string, logger logr.Logger) transport.DakrClient
}

// NewEnvBasedController creates a new environment-based controller
func NewEnvBasedController(
	mgr ctrl.Manager,
	healthManager *health.HealthManager,
	reconcileInterval time.Duration,
	mpaServerPort int,
) (*EnvBasedController, error) {
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
		MpaServerPort:     mpaServerPort,
		HealthManager:     healthManager,
	}

	logger.Info("Checking 1st reconcile interval", "reconcile", reconcileInterval)

	// If no reconcile interval is specified, default to 5 minutes
	if reconcileInterval <= 0 {
		reconcileInterval = 5 * time.Minute
	}

	logger.Info("Checking 2nd reconcile interval", "reconcile", reconcileInterval)

	nodeOperatorMonitor := health.NewNodeOperatorMonitor(
		logger.WithName("node-operator-monitor"),
		clientset,
		&http.Client{Timeout: 5 * time.Second},
	)

	return &EnvBasedController{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		Log:                 logger,
		K8sClient:           clientset,
		DynamicClient:       dynamicClient,
		DiscoveryClient:     discoveryClient,
		ApiExtensions:       apiExtensionClient,
		Reconciler:          reconciler,
		stopCh:              make(chan struct{}),
		reconcileInterval:   reconcileInterval,
		mpaServerPort:       mpaServerPort,
		nodeOperatorMonitor: nodeOperatorMonitor,
		dakrClientFactory:   transport.NewDakrClient,
	}, nil
}

// Start implements the Runnable interface for manager.Add
func (c *EnvBasedController) Start(ctx context.Context) error {
	c.startTime = time.Now()

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

	// Run perioic health check reporting
	go c.runHealthReporting(ctx)

	// Wait for context cancellation
	<-ctx.Done()
	close(c.stopCh)
	c.Log.Info("Stopping environment-based controller")
	return nil
}

// runHealthReporting periodically logs the health status of all registered components
// and sends a heartbeat to dakr via the ReportHealth RPC.
func (c *EnvBasedController) runHealthReporting(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// Send initial heartbeat immediately so dakr sees the operator right away
	c.sendHealthReport(ctx)
	c.sendNodeOperatorHealthReport(ctx)

	for {
		select {
		case <-ticker.C:
			c.sendHealthReport(ctx)
			c.sendNodeOperatorHealthReport(ctx)
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// sendHealthReport logs component health and sends a heartbeat to dakr.
// It builds the report once to keep local logs and the RPC payload consistent.
func (c *EnvBasedController) sendHealthReport(ctx context.Context) {
	report := c.Reconciler.HealthManager.BuildReport()
	for name, status := range report {
		c.Log.Info(
			"Health status report",
			"component",
			name,
			"status",
			status.Status,
			"message",
			status.Message,
			"metadata",
			status.Metadata,
		)
	}

	if c.Reconciler.DakrClient != nil {
		versionInfo := version.Get()
		req := health.BuildHeartbeatRequestFromReport(
			report,
			c.getClusterID(),
			gen.OperatorType_OPERATOR_TYPE_READ,
			versionInfo.String(),
			versionInfo.GitCommit,
			c.startTime,
		)
		if err := c.Reconciler.DakrClient.ReportHealth(ctx, req); err != nil {
			c.Log.Error(err, "Failed to send health heartbeat to dakr, retrying in 5s")
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			if err := c.Reconciler.DakrClient.ReportHealth(ctx, req); err != nil {
				c.Log.Error(err, "Retry also failed for health heartbeat")
			}
		}
	}
}

// sendNodeOperatorHealthReport discovers dzKarp, probes its health, and sends
// a separate ReportHealth heartbeat with OPERATOR_TYPE_NODE to the control plane.
func (c *EnvBasedController) sendNodeOperatorHealthReport(ctx context.Context) {
	report, nodeVersion, nodeCommit, uptimeSince := c.nodeOperatorMonitor.BuildNodeOperatorReport(ctx)
	if report == nil {
		return // dzKarp not found in cluster, nothing to report
	}

	for name, status := range report {
		c.Log.Info(
			"Node operator health status",
			"component",
			name,
			"status",
			status.Status,
			"message",
			status.Message,
			"metadata",
			status.Metadata,
		)
	}

	if c.Reconciler.DakrClient != nil {
		req := health.BuildHeartbeatRequestFromReport(
			report,
			c.getClusterID(),
			gen.OperatorType_OPERATOR_TYPE_NODE,
			nodeVersion,
			nodeCommit,
			uptimeSince,
		)
		if err := c.Reconciler.DakrClient.ReportHealth(ctx, req); err != nil {
			c.Log.Error(err, "Failed to send node operator health heartbeat to dakr, retrying in 5s")
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			if err := c.Reconciler.DakrClient.ReportHealth(ctx, req); err != nil {
				c.Log.Error(err, "Retry also failed for node operator health heartbeat")
			}
		}
	}
}

// getClusterID returns the cluster ID from environment configuration.
func (c *EnvBasedController) getClusterID() string {
	if id := os.Getenv("CLUSTER_ID"); id != "" {
		return id
	}
	return "unknown"
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

	// Resolve clusterIdentifier from the cluster identity Secret if configured.
	// Secret value takes priority over clusterIdentifier in values.yaml/ConfigMap.
	// ConfigMap is mounted as files at /etc/zxporter/config/, not as env vars.
	clusterIdentitySecretName := os.Getenv("CLUSTER_IDENTITY_SECRET_NAME")
	if clusterIdentitySecretName == "" {
		if data, err := os.ReadFile("/etc/zxporter/config/CLUSTER_IDENTITY_SECRET_NAME"); err == nil {
			clusterIdentitySecretName = strings.TrimSpace(string(data))
		}
	}
	if clusterIdentitySecretName != "" {
		identifier, secretErr := c.readClusterIdentifierFromSecret(ctx, clusterIdentitySecretName)
		if secretErr != nil {
			// Case 5: Secret exists but CLUSTER_IDENTIFIER is missing/empty — fail loudly.
			c.Log.Error(secretErr, "Cannot start: cluster identity Secret is misconfigured")
			return secretErr
		}
		if identifier != "" {
			envSpec.Policies.ClusterIdentifier = identifier
		}
	}

	// Handle PAT token exchange if no cluster token is available
	if envSpec.Policies.ClusterToken == "" && envSpec.Policies.PATToken != "" {
		// First try to recover existing stored token
		if storedToken, storedIdentifier := c.tryRecoverStoredClusterToken(ctx); storedToken != "" {
			c.Log.Info("Found existing cluster token in storage, skipping PAT exchange")
			envSpec.Policies.ClusterToken = storedToken
			if storedIdentifier != "" && envSpec.Policies.ClusterIdentifier == "" {
				envSpec.Policies.ClusterIdentifier = storedIdentifier
			}
		} else {
			// Only do PAT exchange if no stored token found
			c.Log.Info("No stored cluster token found, attempting PAT token exchange")

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
			tempClient := c.dakrClientFactory(dakrURL, "", c.Log)

			// Exchange PAT for cluster token — use ReattachCluster when clusterIdentifier is set
			// so reinstalls find the same cluster instead of creating a new one.
			var token, clusterId string
			var err error
			if envSpec.Policies.ClusterIdentifier != "" {
				token, clusterId, err = tempClient.ReattachCluster(ctx, envSpec.Policies.PATToken, envSpec.Policies.ClusterIdentifier, clusterName, k8sProvider)
			} else {
				token, clusterId, err = tempClient.ExchangePATForClusterToken(ctx, envSpec.Policies.PATToken, clusterName, k8sProvider)
			}
			if err != nil {
				c.Log.Error(err, "Failed to exchange PAT for cluster token")
				return fmt.Errorf("failed to exchange PAT for cluster token: %w", err)
			}
			c.Log.Info("Successfully obtained cluster token", "clusterId", clusterId)
			envSpec.Policies.ClusterToken = token

			// Case 3: both clusterIdentifier and identity Secret were absent.
			// The API returned a clusterId — persist it to the identity Secret so future
			// restarts/reinstalls reuse the same cluster identity instead of creating a new one.
			if envSpec.Policies.ClusterIdentifier == "" && clusterId != "" && clusterIdentitySecretName != "" {
				envSpec.Policies.ClusterIdentifier = clusterId
				if err := c.persistClusterIdentifierToIdentitySecret(ctx, clusterIdentitySecretName, clusterId); err != nil {
					c.Log.Error(err, "Failed to persist cluster identifier to identity Secret")
					// Non-fatal: token is in memory, operator can still run
				}
			}

			// Persist the token (and identifier if set) to ConfigMap or Secret based on configuration
			if err := c.persistClusterToken(ctx, token, envSpec.Policies.ClusterIdentifier); err != nil {
				c.Log.Error(err, "Failed to persist cluster token")
				// Continue anyway - the token is in memory
			}
		}
	}

	// Create dakr client and sender
	var dakrClient transport.DakrClient
	if envSpec.Policies.DakrURL != "" && envSpec.Policies.ClusterToken != "" {
		dakrClient = transport.NewDakrClient(
			envSpec.Policies.DakrURL,
			envSpec.Policies.ClusterToken,
			c.Log,
		)
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

// persistClusterToken persists the cluster token (and optional identifier) to ConfigMap or Secret based on configuration
func (c *EnvBasedController) persistClusterToken(ctx context.Context, token, identifier string) error {
	if c.shouldUseSecretStorage() {
		return c.persistClusterTokenToSecret(ctx, token, identifier)
	}
	return c.persistClusterTokenToConfigMap(ctx, token, identifier)
}

// persistClusterTokenToConfigMap persists the cluster token (and optional identifier) to the ConfigMap
func (c *EnvBasedController) persistClusterTokenToConfigMap(ctx context.Context, token, identifier string) error {
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
	configMap, err := c.K8sClient.CoreV1().
		ConfigMaps(namespace).
		Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	// Update the CLUSTER_TOKEN in the ConfigMap data
	if configMap.Data == nil {
		configMap.Data = make(map[string]string)
	}
	configMap.Data["CLUSTER_TOKEN"] = token
	if identifier != "" {
		configMap.Data["CLUSTER_IDENTIFIER"] = identifier
	}

	// Update the ConfigMap
	_, err = c.K8sClient.CoreV1().
		ConfigMaps(namespace).
		Update(ctx, configMap, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ConfigMap with cluster token: %w", err)
	}

	c.Log.Info("Successfully persisted cluster token to ConfigMap", "configMap", configMapName)
	return nil
}

// persistClusterTokenToSecret persists the cluster token (and optional identifier) to a Kubernetes Secret
func (c *EnvBasedController) persistClusterTokenToSecret(ctx context.Context, token, identifier string) error {
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
	secret, err := c.K8sClient.CoreV1().
		Secrets(namespace).
		Get(ctx, runtimeSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Create new Secret if it doesn't exist
			secretData := map[string][]byte{
				"CLUSTER_TOKEN": []byte(token),
			}
			if identifier != "" {
				secretData["CLUSTER_IDENTIFIER"] = []byte(identifier)
			}
			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      runtimeSecretName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/name":      "zxporter",
						"app.kubernetes.io/component": "token-storage",
					},
				},
				Type: corev1.SecretTypeOpaque,
				Data: secretData,
			}
			_, err = c.K8sClient.CoreV1().
				Secrets(namespace).
				Create(ctx, secret, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create Secret: %w", err)
			}
			c.Log.Info(
				"Successfully created Secret with cluster token",
				"secret",
				runtimeSecretName,
			)
		} else {
			return fmt.Errorf("failed to get Secret: %w", err)
		}
	} else {
		// Update existing Secret
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data["CLUSTER_TOKEN"] = []byte(token)
		if identifier != "" {
			secret.Data["CLUSTER_IDENTIFIER"] = []byte(identifier)
		}

		_, err = c.K8sClient.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update Secret with cluster token: %w", err)
		}
		c.Log.Info("Successfully updated Secret with cluster token", "secret", runtimeSecretName)
	}

	return nil
}

// tryRecoverStoredClusterToken attempts to read a previously stored cluster token and identifier.
// Returns (token, identifier) — identifier may be empty if not previously persisted.
func (c *EnvBasedController) tryRecoverStoredClusterToken(ctx context.Context) (string, string) {
	if c.shouldUseSecretStorage() {
		return c.readClusterTokenFromSecret(ctx)
	}
	return c.readClusterTokenFromConfigMap(ctx)
}

// readClusterTokenFromSecret reads cluster token and identifier from runtime secret.
// Returns (token, identifier) — identifier may be empty if not previously persisted.
func (c *EnvBasedController) readClusterTokenFromSecret(ctx context.Context) (string, string) {
	// Get namespace from environment variable or use default
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		// Try to read from service account namespace file
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			namespace = strings.TrimSpace(string(data))
		} else {
			// Fall back to default if all else fails
			namespace = "devzero-zxporter"
		}
	}

	// Get runtime Secret name from environment variable with fallback to default
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

	// Try to read the Secret
	secret, err := c.K8sClient.CoreV1().
		Secrets(namespace).
		Get(ctx, runtimeSecretName, metav1.GetOptions{})
	if err != nil {
		// Log the error but don't fail - this is a recovery attempt
		c.Log.Info("Could not read cluster token from Secret", "error", err.Error(), "secret", runtimeSecretName)
		return "", ""
	}

	// Extract CLUSTER_TOKEN (and optional CLUSTER_IDENTIFIER) from Secret data
	if secret.Data != nil {
		if tokenBytes, exists := secret.Data["CLUSTER_TOKEN"]; exists {
			token := strings.TrimSpace(string(tokenBytes))
			if token != "" {
				identifier := strings.TrimSpace(string(secret.Data["CLUSTER_IDENTIFIER"]))
				c.Log.Info("Successfully recovered cluster token from Secret", "secret", runtimeSecretName)
				return token, identifier
			}
		}
	}

	c.Log.Info("No cluster token found in Secret", "secret", runtimeSecretName)
	return "", ""
}

// readClusterTokenFromConfigMap reads cluster token and identifier from configmap.
// Returns (token, identifier) — identifier may be empty if not previously persisted.
func (c *EnvBasedController) readClusterTokenFromConfigMap(ctx context.Context) (string, string) {
	// Get namespace from environment variable or use default
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		// Try to read from service account namespace file
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			namespace = strings.TrimSpace(string(data))
		} else {
			// Fall back to default if all else fails
			namespace = "devzero-zxporter"
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

	// Try to read the ConfigMap
	configMap, err := c.K8sClient.CoreV1().
		ConfigMaps(namespace).
		Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		// Log the error but don't fail - this is a recovery attempt
		c.Log.Info("Could not read cluster token from ConfigMap", "error", err.Error(), "configMap", configMapName)
		return "", ""
	}

	// Extract CLUSTER_TOKEN (and optional CLUSTER_IDENTIFIER) from ConfigMap data
	if configMap.Data != nil {
		if token, exists := configMap.Data["CLUSTER_TOKEN"]; exists {
			token = strings.TrimSpace(token)
			if token != "" {
				identifier := strings.TrimSpace(configMap.Data["CLUSTER_IDENTIFIER"])
				c.Log.Info("Successfully recovered cluster token from ConfigMap", "configMap", configMapName)
				return token, identifier
			}
		}
	}

	c.Log.Info("No cluster token found in ConfigMap", "configMap", configMapName)
	return "", ""
}

// readClusterIdentifierFromSecret reads CLUSTER_IDENTIFIER from the cluster identity Secret.
// Returns:
//   - (identifier, nil)  — Secret found and key is non-empty
//   - ("", nil)          — Secret not found; caller should fall back to values.yaml
//   - ("", error)        — Secret exists but CLUSTER_IDENTIFIER key is missing or empty (case 5)
func (c *EnvBasedController) readClusterIdentifierFromSecret(ctx context.Context, secretName string) (string, error) {
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			namespace = strings.TrimSpace(string(data))
		} else {
			namespace = "devzero-zxporter"
		}
	}

	secret, err := c.K8sClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Secret doesn't exist yet — fall through to values.yaml / operator auto-create
			c.Log.Info("Cluster identity Secret not found, will fall back to values.yaml", "secret", secretName)
			return "", nil
		}
		return "", fmt.Errorf("failed to read cluster identity Secret %q: %w", secretName, err)
	}

	if secret.Data != nil {
		if val, exists := secret.Data["CLUSTER_IDENTIFIER"]; exists {
			if identifier := strings.TrimSpace(string(val)); identifier != "" {
				c.Log.Info("Read clusterIdentifier from cluster identity Secret",
					"secret", secretName, "identifier", identifier)
				return identifier, nil
			}
		}
	}

	// Case 5: Secret exists but CLUSTER_IDENTIFIER is missing or empty — this is a configuration error.
	return "", fmt.Errorf("cluster identity Secret %q exists but CLUSTER_IDENTIFIER key is missing or empty; "+
		"populate the key or delete the Secret so the operator can recreate it", secretName)
}

// persistClusterIdentifierToIdentitySecret writes CLUSTER_IDENTIFIER into the cluster identity Secret.
// Creates the Secret if it does not exist (case 3: both absent — operator auto-creates after token exchange).
func (c *EnvBasedController) persistClusterIdentifierToIdentitySecret(ctx context.Context, secretName, identifier string) error {
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			namespace = strings.TrimSpace(string(data))
		} else {
			namespace = "devzero-zxporter"
		}
	}

	existing, err := c.K8sClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get cluster identity Secret %q: %w", secretName, err)
		}
		// Create new Secret
		newSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: namespace,
				Annotations: map[string]string{
					"helm.sh/resource-policy": "keep",
				},
				Labels: map[string]string{
					"app.kubernetes.io/name":      "zxporter",
					"app.kubernetes.io/component": "cluster-identity",
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"CLUSTER_IDENTIFIER": []byte(identifier),
			},
		}
		if _, err = c.K8sClient.CoreV1().Secrets(namespace).Create(ctx, newSecret, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create cluster identity Secret %q: %w", secretName, err)
		}
		c.Log.Info("Created cluster identity Secret", "secret", secretName, "identifier", identifier)
		return nil
	}

	// Update existing
	if existing.Data == nil {
		existing.Data = make(map[string][]byte)
	}
	existing.Data["CLUSTER_IDENTIFIER"] = []byte(identifier)
	if _, err = c.K8sClient.CoreV1().Secrets(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update cluster identity Secret %q: %w", secretName, err)
	}
	c.Log.Info("Updated cluster identity Secret", "secret", secretName, "identifier", identifier)
	return nil
}
