// internal/util/env.go
package util

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// Configuration environment variables
const (
	// CLUSTER_TOKEN is the cluster token used to authenticate as a cluster
	// Default value: ""
	_ENV_CLUSTER_TOKEN = "CLUSTER_TOKEN"

	// PULSE_URL is the URL of the Pulse service.
	// Default value: ""
	_ENV_PULSE_URL = "PULSE_URL"

	// COLLECTION_FREQUENCY is how often to collect resource usage metrics.
	// Default value: 10s
	_ENV_COLLECTION_FREQUENCY = "COLLECTION_FREQUENCY"

	// BUFFER_SIZE is the size of the sender buffer.
	// Default value: 1000
	_ENV_BUFFER_SIZE = "BUFFER_SIZE"

	// EXCLUDED_NAMESPACES are namespaces to exclude from collection.
	// This is a comma-separated list.
	// Default value: []
	_ENV_EXCLUDED_NAMESPACES = "EXCLUDED_NAMESPACES"

	// EXCLUDED_NODES are nodes to exclude from collection.
	// This is a comma-separated list.
	// Default value: []
	_ENV_EXCLUDED_NODES = "EXCLUDED_NODES"

	// TARGET_NAMESPACES are namespaces to include in collection (empty means all).
	// This is a comma-separated list.
	// Default value: [] (all namespaces)
	_ENV_TARGET_NAMESPACES = "TARGET_NAMESPACES"
)

// EnvPolicyConfig holds configuration that can be set via environment variables
type EnvPolicyConfig struct {
	// ClusterToken is the token used to authenticate as a cluster
	ClusterToken string

	// PulseURL is the URL of the Pulse service
	PulseURL string

	// Frequency is how often to collect resource usage metrics
	Frequency time.Duration

	// BufferSize is the size of the sender buffer
	BufferSize int

	// ExcludedNamespaces are namespaces to exclude from collection
	ExcludedNamespaces []string

	// ExcludedNodes are nodes to exclude from collection
	ExcludedNodes []string

	// TargetNamespaces are namespaces to include in collection (empty means all)
	TargetNamespaces []string
}

// LoadEnvPolicyConfig loads configuration from environment variables
func LoadEnvPolicyConfig(logger logr.Logger) *EnvPolicyConfig {
	config := &EnvPolicyConfig{}

	// Load cluster token
	if token := os.Getenv(_ENV_CLUSTER_TOKEN); token != "" {
		config.ClusterToken = token
		logger.Info("Loaded ClusterToken from environment")
	}

	// Load pulse URL
	if url := os.Getenv(_ENV_PULSE_URL); url != "" {
		config.PulseURL = url
		logger.Info("Loaded PulseURL from environment", "url", url)
	}

	// Load collection frequency
	if freqStr := os.Getenv(_ENV_COLLECTION_FREQUENCY); freqStr != "" {
		if freq, err := time.ParseDuration(freqStr); err == nil {
			config.Frequency = freq
			logger.Info("Loaded collection frequency from environment", "frequency", freq)
		} else {
			logger.Error(err, "Failed to parse COLLECTION_FREQUENCY environment variable", "value", freqStr)
		}
	}

	// Load buffer size
	if sizeStr := os.Getenv(_ENV_BUFFER_SIZE); sizeStr != "" {
		if size, err := strconv.Atoi(sizeStr); err == nil {
			config.BufferSize = size
			logger.Info("Loaded buffer size from environment", "size", size)
		} else {
			logger.Error(err, "Failed to parse BUFFER_SIZE environment variable", "value", sizeStr)
		}
	}

	// Load excluded namespaces
	if nsStr := os.Getenv(_ENV_EXCLUDED_NAMESPACES); nsStr != "" {
		config.ExcludedNamespaces = parseCommaList(nsStr)
		logger.Info("Loaded excluded namespaces from environment", "namespaces", config.ExcludedNamespaces)
	}

	// Load excluded nodes
	if nodesStr := os.Getenv(_ENV_EXCLUDED_NODES); nodesStr != "" {
		config.ExcludedNodes = parseCommaList(nodesStr)
		logger.Info("Loaded excluded nodes from environment", "nodes", config.ExcludedNodes)
	}

	// Load target namespaces
	if nsStr := os.Getenv(_ENV_TARGET_NAMESPACES); nsStr != "" {
		config.TargetNamespaces = parseCommaList(nsStr)
		logger.Info("Loaded target namespaces from environment", "namespaces", config.TargetNamespaces)
	}

	return config
}

// parseCommaList parses a comma-separated list of strings
func parseCommaList(input string) []string {
	if input == "" {
		return nil
	}

	items := strings.Split(input, ",")
	result := make([]string, 0, len(items))

	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}

	return result
}

// MergeWithCRPolicy merges environment config with CR policy, with environment taking precedence
func (e *EnvPolicyConfig) MergeWithCRPolicy(
	targetNamespaces []string,
	excludedNamespaces []string,
	excludedNodes []string,
	pulseURL string,
	frequencyStr string,
	bufferSize int,
	clusterToken string,
) ([]string, []string, []string, string, time.Duration, int, string) {

	// Initialize default values from CR
	mergedTargetNamespaces := targetNamespaces
	mergedExcludedNamespaces := excludedNamespaces
	mergedExcludedNodes := excludedNodes
	mergedPulseURL := pulseURL
	mergedClusterToken := clusterToken

	// Parse frequency
	var mergedFrequency time.Duration
	if frequencyStr != "" {
		if freq, err := time.ParseDuration(frequencyStr); err == nil {
			mergedFrequency = freq
		}
	}

	mergedBufferSize := bufferSize

	// Override with environment values if set
	if len(e.TargetNamespaces) > 0 {
		mergedTargetNamespaces = e.TargetNamespaces
	}

	if len(e.ExcludedNamespaces) > 0 {
		mergedExcludedNamespaces = e.ExcludedNamespaces
	}

	if len(e.ExcludedNodes) > 0 {
		mergedExcludedNodes = e.ExcludedNodes
	}

	if e.PulseURL != "" {
		mergedPulseURL = e.PulseURL
	}

	if e.ClusterToken != "" {
		mergedClusterToken = e.ClusterToken
	}

	if e.Frequency > 0 {
		mergedFrequency = e.Frequency
	}

	if e.BufferSize > 0 {
		mergedBufferSize = e.BufferSize
	}

	return mergedTargetNamespaces, mergedExcludedNamespaces, mergedExcludedNodes, mergedPulseURL, mergedFrequency, mergedBufferSize, mergedClusterToken
}
