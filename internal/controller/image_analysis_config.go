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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/devzero-inc/zxporter/internal/util"
)

// Environment variable keys for image analysis configuration.
const (
	// Enable/Disable
	_ENV_IMAGE_ANALYSIS_ENABLED = "IMAGE_ANALYSIS_ENABLED"

	// Schedule
	_ENV_IMAGE_ANALYSIS_INTERVAL_DAYS   = "IMAGE_ANALYSIS_INTERVAL_DAYS"
	_ENV_IMAGE_ANALYSIS_CRON_EXPRESSION = "IMAGE_ANALYSIS_CRON_EXPRESSION"

	// Job execution tuning
	_ENV_IMAGE_ANALYSIS_MAX_CONCURRENT_JOBS         = "IMAGE_ANALYSIS_MAX_CONCURRENT_JOBS"
	_ENV_IMAGE_ANALYSIS_MAX_JOBS_PER_NODE            = "IMAGE_ANALYSIS_MAX_JOBS_PER_NODE"
	_ENV_IMAGE_ANALYSIS_BATCH_SIZE                   = "IMAGE_ANALYSIS_BATCH_SIZE"
	_ENV_IMAGE_ANALYSIS_JOB_TIMEOUT_MINUTES          = "IMAGE_ANALYSIS_JOB_TIMEOUT_MINUTES"
	_ENV_IMAGE_ANALYSIS_MAX_RETRIES                  = "IMAGE_ANALYSIS_MAX_RETRIES"
	_ENV_IMAGE_ANALYSIS_JOB_NAMESPACE                = "IMAGE_ANALYSIS_JOB_NAMESPACE"
	_ENV_IMAGE_ANALYSIS_ANALYZER_IMAGE               = "IMAGE_ANALYSIS_ANALYZER_IMAGE"
	_ENV_IMAGE_ANALYSIS_JOB_CPU_REQUEST              = "IMAGE_ANALYSIS_JOB_CPU_REQUEST"
	_ENV_IMAGE_ANALYSIS_JOB_CPU_LIMIT                = "IMAGE_ANALYSIS_JOB_CPU_LIMIT"
	_ENV_IMAGE_ANALYSIS_JOB_MEMORY_REQUEST           = "IMAGE_ANALYSIS_JOB_MEMORY_REQUEST"
	_ENV_IMAGE_ANALYSIS_JOB_MEMORY_LIMIT             = "IMAGE_ANALYSIS_JOB_MEMORY_LIMIT"
	_ENV_IMAGE_ANALYSIS_WORKSPACE_SIZE_LIMIT         = "IMAGE_ANALYSIS_WORKSPACE_SIZE_LIMIT"
	_ENV_IMAGE_ANALYSIS_REGISTRY_PULL_RATE_PER_MINUTE = "IMAGE_ANALYSIS_REGISTRY_PULL_RATE_PER_MINUTE"

	// Job tolerations
	_ENV_IMAGE_ANALYSIS_JOB_TOLERATIONS = "IMAGE_ANALYSIS_JOB_TOLERATIONS"

	// Image source preferences
	_ENV_IMAGE_ANALYSIS_PREFER_LOCAL            = "IMAGE_ANALYSIS_PREFER_LOCAL"
	_ENV_IMAGE_ANALYSIS_CONTAINERD_SOCKET_PATH  = "IMAGE_ANALYSIS_CONTAINERD_SOCKET_PATH"
	_ENV_IMAGE_ANALYSIS_CONTAINERD_NAMESPACE    = "IMAGE_ANALYSIS_CONTAINERD_NAMESPACE"
	_ENV_IMAGE_ANALYSIS_FALLBACK_TO_REMOTE      = "IMAGE_ANALYSIS_FALLBACK_TO_REMOTE"
	_ENV_IMAGE_ANALYSIS_REMOTE_PULL_TIMEOUT     = "IMAGE_ANALYSIS_REMOTE_PULL_TIMEOUT"
	_ENV_IMAGE_ANALYSIS_REGISTRY_AUTH_SECRET     = "IMAGE_ANALYSIS_REGISTRY_AUTH_SECRET"

	// Targeting
	_ENV_IMAGE_ANALYSIS_TARGET_NAMESPACES   = "IMAGE_ANALYSIS_TARGET_NAMESPACES"
	_ENV_IMAGE_ANALYSIS_EXCLUDED_NAMESPACES = "IMAGE_ANALYSIS_EXCLUDED_NAMESPACES"
	_ENV_IMAGE_ANALYSIS_EXCLUDED_IMAGES     = "IMAGE_ANALYSIS_EXCLUDED_IMAGES"
	_ENV_IMAGE_ANALYSIS_INCLUDED_IMAGES     = "IMAGE_ANALYSIS_INCLUDED_IMAGES"

	// Thresholds
	_ENV_IMAGE_ANALYSIS_LOWEST_EFFICIENCY           = "IMAGE_ANALYSIS_LOWEST_EFFICIENCY"
	_ENV_IMAGE_ANALYSIS_HIGHEST_WASTED_BYTES        = "IMAGE_ANALYSIS_HIGHEST_WASTED_BYTES"
	_ENV_IMAGE_ANALYSIS_HIGHEST_USER_WASTED_PERCENT = "IMAGE_ANALYSIS_HIGHEST_USER_WASTED_PERCENT"

	// Cleanup
	_ENV_IMAGE_ANALYSIS_RESULT_RETENTION_DAYS = "IMAGE_ANALYSIS_RESULT_RETENTION_DAYS"
)

// ImageAnalysisConfig holds all configuration for the image analysis controller.
type ImageAnalysisConfig struct {
	// Enable/Disable
	Enabled bool

	// Schedule
	IntervalDays   int
	CronExpression string

	// Job execution tuning
	MaxConcurrentJobs         int
	MaxJobsPerNode            int
	BatchSize                 int
	JobTimeoutMinutes         int
	MaxRetries                int
	JobNamespace              string
	AnalyzerImage             string
	JobCPURequest             string
	JobCPULimit               string
	JobMemoryRequest          string
	JobMemoryLimit            string
	WorkspaceSizeLimit        string
	RegistryPullRatePerMinute int
	JobTolerations            []corev1.Toleration

	// Image source preferences
	PreferLocal          bool
	ContainerdSocketPath string
	ContainerdNamespace  string
	FallbackToRemote     bool
	RemotePullTimeout    string
	RegistryAuthSecret   string

	// Targeting
	TargetNamespaces   []string
	ExcludedNamespaces []string
	ExcludedImages     []string
	IncludedImages     []string

	// Thresholds
	LowestEfficiency         float64
	HighestWastedBytes       string
	HighestUserWastedPercent float64

	// Cleanup
	ResultRetentionDays int
}

// DefaultImageAnalysisConfig returns the config with all defaults applied.
func DefaultImageAnalysisConfig() ImageAnalysisConfig {
	return ImageAnalysisConfig{
		Enabled:                  true,
		IntervalDays:             7,
		MaxConcurrentJobs:        10,
		MaxJobsPerNode:           1,
		BatchSize:                10,
		JobTimeoutMinutes:        30,
		MaxRetries:               2,
		JobNamespace:             "devzero-zxporter",
		AnalyzerImage:            "devzeroinc/image-analyzer:v1",
		JobCPURequest:            "500m",
		JobCPULimit:              "1",
		JobMemoryRequest:         "1Gi",
		JobMemoryLimit:           "4Gi",
		WorkspaceSizeLimit:       "10Gi",
		RegistryPullRatePerMinute: 30,
		PreferLocal:              true,
		ContainerdSocketPath:     "/run/containerd/containerd.sock",
		ContainerdNamespace:      "k8s.io",
		FallbackToRemote:         true,
		RemotePullTimeout:        "5m",
		ExcludedNamespaces:       []string{"kube-system", "kube-public"},
		ExcludedImages:           []string{"registry.k8s.io/pause:*"},
		LowestEfficiency:         0.90,
		HighestWastedBytes:       "50MB",
		HighestUserWastedPercent: 0.20,
		ResultRetentionDays:      30,
	}
}

// LoadImageAnalysisConfigFromEnv loads image analysis configuration from environment
// variables with fallback to /etc/zxporter/config/<KEY> files. Unset values use defaults.
func LoadImageAnalysisConfigFromEnv() (ImageAnalysisConfig, error) {
	cfg := DefaultImageAnalysisConfig()

	// Helper to split comma-separated lists
	splitCSV := func(envKey string) []string {
		if raw := util.GetEnv(envKey); raw != "" {
			parts := strings.Split(raw, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			return parts
		}
		return nil
	}

	// Helper to parse int with default
	parseInt := func(envKey string, defaultVal int) (int, error) {
		if raw := util.GetEnv(envKey); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil {
				return 0, fmt.Errorf("invalid integer for %s: %w", envKey, err)
			}
			return v, nil
		}
		return defaultVal, nil
	}

	// Helper to parse bool with default
	parseBool := func(envKey string, defaultVal bool) (bool, error) {
		if raw := util.GetEnv(envKey); raw != "" {
			v, err := strconv.ParseBool(raw)
			if err != nil {
				return false, fmt.Errorf("invalid boolean for %s: %w", envKey, err)
			}
			return v, nil
		}
		return defaultVal, nil
	}

	// Helper to parse float64 with default
	parseFloat := func(envKey string, defaultVal float64) (float64, error) {
		if raw := util.GetEnv(envKey); raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid float for %s: %w", envKey, err)
			}
			return v, nil
		}
		return defaultVal, nil
	}

	// Helper to read string with default
	readString := func(envKey string, defaultVal string) string {
		if raw := util.GetEnv(envKey); raw != "" {
			return raw
		}
		return defaultVal
	}

	var err error

	// === Enable/Disable ===
	if cfg.Enabled, err = parseBool(_ENV_IMAGE_ANALYSIS_ENABLED, cfg.Enabled); err != nil {
		return cfg, err
	}

	// === Schedule ===
	if cfg.IntervalDays, err = parseInt(_ENV_IMAGE_ANALYSIS_INTERVAL_DAYS, cfg.IntervalDays); err != nil {
		return cfg, err
	}
	cfg.CronExpression = readString(_ENV_IMAGE_ANALYSIS_CRON_EXPRESSION, cfg.CronExpression)

	// === Job execution tuning ===
	if cfg.MaxConcurrentJobs, err = parseInt(_ENV_IMAGE_ANALYSIS_MAX_CONCURRENT_JOBS, cfg.MaxConcurrentJobs); err != nil {
		return cfg, err
	}
	if cfg.MaxJobsPerNode, err = parseInt(_ENV_IMAGE_ANALYSIS_MAX_JOBS_PER_NODE, cfg.MaxJobsPerNode); err != nil {
		return cfg, err
	}
	if cfg.BatchSize, err = parseInt(_ENV_IMAGE_ANALYSIS_BATCH_SIZE, cfg.BatchSize); err != nil {
		return cfg, err
	}
	if cfg.JobTimeoutMinutes, err = parseInt(_ENV_IMAGE_ANALYSIS_JOB_TIMEOUT_MINUTES, cfg.JobTimeoutMinutes); err != nil {
		return cfg, err
	}
	if cfg.MaxRetries, err = parseInt(_ENV_IMAGE_ANALYSIS_MAX_RETRIES, cfg.MaxRetries); err != nil {
		return cfg, err
	}
	cfg.JobNamespace = readString(_ENV_IMAGE_ANALYSIS_JOB_NAMESPACE, cfg.JobNamespace)
	cfg.AnalyzerImage = readString(_ENV_IMAGE_ANALYSIS_ANALYZER_IMAGE, cfg.AnalyzerImage)
	cfg.JobCPURequest = readString(_ENV_IMAGE_ANALYSIS_JOB_CPU_REQUEST, cfg.JobCPURequest)
	cfg.JobCPULimit = readString(_ENV_IMAGE_ANALYSIS_JOB_CPU_LIMIT, cfg.JobCPULimit)
	cfg.JobMemoryRequest = readString(_ENV_IMAGE_ANALYSIS_JOB_MEMORY_REQUEST, cfg.JobMemoryRequest)
	cfg.JobMemoryLimit = readString(_ENV_IMAGE_ANALYSIS_JOB_MEMORY_LIMIT, cfg.JobMemoryLimit)
	cfg.WorkspaceSizeLimit = readString(_ENV_IMAGE_ANALYSIS_WORKSPACE_SIZE_LIMIT, cfg.WorkspaceSizeLimit)
	if cfg.RegistryPullRatePerMinute, err = parseInt(_ENV_IMAGE_ANALYSIS_REGISTRY_PULL_RATE_PER_MINUTE, cfg.RegistryPullRatePerMinute); err != nil {
		return cfg, err
	}

	// === Job tolerations (JSON array) ===
	if raw := util.GetEnv(_ENV_IMAGE_ANALYSIS_JOB_TOLERATIONS); raw != "" {
		var tolerations []corev1.Toleration
		if err := json.Unmarshal([]byte(raw), &tolerations); err != nil {
			return cfg, fmt.Errorf("invalid JSON for %s: %w", _ENV_IMAGE_ANALYSIS_JOB_TOLERATIONS, err)
		}
		cfg.JobTolerations = tolerations
	}

	// === Image source preferences ===
	if cfg.PreferLocal, err = parseBool(_ENV_IMAGE_ANALYSIS_PREFER_LOCAL, cfg.PreferLocal); err != nil {
		return cfg, err
	}
	cfg.ContainerdSocketPath = readString(_ENV_IMAGE_ANALYSIS_CONTAINERD_SOCKET_PATH, cfg.ContainerdSocketPath)
	cfg.ContainerdNamespace = readString(_ENV_IMAGE_ANALYSIS_CONTAINERD_NAMESPACE, cfg.ContainerdNamespace)
	if cfg.FallbackToRemote, err = parseBool(_ENV_IMAGE_ANALYSIS_FALLBACK_TO_REMOTE, cfg.FallbackToRemote); err != nil {
		return cfg, err
	}
	cfg.RemotePullTimeout = readString(_ENV_IMAGE_ANALYSIS_REMOTE_PULL_TIMEOUT, cfg.RemotePullTimeout)
	cfg.RegistryAuthSecret = readString(_ENV_IMAGE_ANALYSIS_REGISTRY_AUTH_SECRET, cfg.RegistryAuthSecret)

	// === Targeting ===
	if ns := splitCSV(_ENV_IMAGE_ANALYSIS_TARGET_NAMESPACES); ns != nil {
		cfg.TargetNamespaces = ns
	}
	if ns := splitCSV(_ENV_IMAGE_ANALYSIS_EXCLUDED_NAMESPACES); ns != nil {
		cfg.ExcludedNamespaces = ns
	}
	if imgs := splitCSV(_ENV_IMAGE_ANALYSIS_EXCLUDED_IMAGES); imgs != nil {
		cfg.ExcludedImages = imgs
	}
	if imgs := splitCSV(_ENV_IMAGE_ANALYSIS_INCLUDED_IMAGES); imgs != nil {
		cfg.IncludedImages = imgs
	}

	// === Thresholds ===
	if cfg.LowestEfficiency, err = parseFloat(_ENV_IMAGE_ANALYSIS_LOWEST_EFFICIENCY, cfg.LowestEfficiency); err != nil {
		return cfg, err
	}
	cfg.HighestWastedBytes = readString(_ENV_IMAGE_ANALYSIS_HIGHEST_WASTED_BYTES, cfg.HighestWastedBytes)
	if cfg.HighestUserWastedPercent, err = parseFloat(_ENV_IMAGE_ANALYSIS_HIGHEST_USER_WASTED_PERCENT, cfg.HighestUserWastedPercent); err != nil {
		return cfg, err
	}

	// === Cleanup ===
	if cfg.ResultRetentionDays, err = parseInt(_ENV_IMAGE_ANALYSIS_RESULT_RETENTION_DAYS, cfg.ResultRetentionDays); err != nil {
		return cfg, err
	}

	// === Validate resource quantities to catch bad values at load time (not at Job creation) ===
	for _, check := range []struct {
		name, value string
	}{
		{"JobCPURequest", cfg.JobCPURequest},
		{"JobCPULimit", cfg.JobCPULimit},
		{"JobMemoryRequest", cfg.JobMemoryRequest},
		{"JobMemoryLimit", cfg.JobMemoryLimit},
		{"WorkspaceSizeLimit", cfg.WorkspaceSizeLimit},
	} {
		if _, err := resource.ParseQuantity(check.value); err != nil {
			return cfg, fmt.Errorf("invalid resource quantity for %s (%q): %w", check.name, check.value, err)
		}
	}

	return cfg, nil
}
