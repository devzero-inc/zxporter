// internal/util/env.go
package util

import (
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"
)

// EnvPolicyConfig holds configuration that can be set via environment variables
type EnvPolicyConfig struct {
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

	// Load pulse URL
	if url := os.Getenv("PULSE_URL"); url != "" {
		config.PulseURL = url
		logger.Info("Loaded PulseURL from environment", "url", url)
	}

	// Load collection frequency
	if freqStr := os.Getenv("COLLECTION_FREQUENCY"); freqStr != "" {
		if freq, err := time.ParseDuration(freqStr); err == nil {
			config.Frequency = freq
			logger.Info("Loaded collection frequency from environment", "frequency", freq)
		} else {
			logger.Error(err, "Failed to parse COLLECTION_FREQUENCY environment variable", "value", freqStr)
		}
	}

	// Load buffer size
	if sizeStr := os.Getenv("BUFFER_SIZE"); sizeStr != "" {
		if size, err := strconv.Atoi(sizeStr); err == nil {
			config.BufferSize = size
			logger.Info("Loaded buffer size from environment", "size", size)
		} else {
			logger.Error(err, "Failed to parse BUFFER_SIZE environment variable", "value", sizeStr)
		}
	}

	// Load excluded namespaces
	if nsStr := os.Getenv("EXCLUDED_NAMESPACES"); nsStr != "" {
		config.ExcludedNamespaces = parseCommaList(nsStr)
		logger.Info("Loaded excluded namespaces from environment", "namespaces", config.ExcludedNamespaces)
	}

	// Load excluded nodes
	if nodesStr := os.Getenv("EXCLUDED_NODES"); nodesStr != "" {
		config.ExcludedNodes = parseCommaList(nodesStr)
		logger.Info("Loaded excluded nodes from environment", "nodes", config.ExcludedNodes)
	}

	// Load target namespaces
	if nsStr := os.Getenv("TARGET_NAMESPACES"); nsStr != "" {
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

	items := []string{}
	current := ""

	for i := 0; i < len(input); i++ {
		if input[i] == ',' {
			if current != "" {
				items = append(items, current)
				current = ""
			}
		} else {
			current += string(input[i])
		}
	}

	if current != "" {
		items = append(items, current)
	}

	return items
}

// MergeWithCRPolicy merges environment config with CR policy, with environment taking precedence
func (e *EnvPolicyConfig) MergeWithCRPolicy(
	targetNamespaces []string,
	excludedNamespaces []string,
	excludedNodes []string,
	pulseURL string,
	frequencyStr string,
	bufferSize int,
) ([]string, []string, []string, string, time.Duration, int) {

	// Initialize default values from CR
	mergedTargetNamespaces := targetNamespaces
	mergedExcludedNamespaces := excludedNamespaces
	mergedExcludedNodes := excludedNodes
	mergedPulseURL := pulseURL

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

	if e.Frequency > 0 {
		mergedFrequency = e.Frequency
	}

	if e.BufferSize > 0 {
		mergedBufferSize = e.BufferSize
	}

	return mergedTargetNamespaces, mergedExcludedNamespaces, mergedExcludedNodes, mergedPulseURL, mergedFrequency, mergedBufferSize
}
