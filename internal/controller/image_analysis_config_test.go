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
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clearImageAnalysisEnvVars(t *testing.T) {
	t.Helper()
	envVars := []string{
		_ENV_IMAGE_ANALYSIS_ENABLED,
		_ENV_IMAGE_ANALYSIS_INTERVAL_DAYS,
		_ENV_IMAGE_ANALYSIS_CRON_EXPRESSION,
		_ENV_IMAGE_ANALYSIS_MAX_CONCURRENT_JOBS,
		_ENV_IMAGE_ANALYSIS_MAX_JOBS_PER_NODE,
		_ENV_IMAGE_ANALYSIS_BATCH_SIZE,
		_ENV_IMAGE_ANALYSIS_JOB_TIMEOUT_MINUTES,
		_ENV_IMAGE_ANALYSIS_MAX_RETRIES,
		_ENV_IMAGE_ANALYSIS_JOB_NAMESPACE,
		_ENV_IMAGE_ANALYSIS_ANALYZER_IMAGE,
		_ENV_IMAGE_ANALYSIS_JOB_CPU_REQUEST,
		_ENV_IMAGE_ANALYSIS_JOB_CPU_LIMIT,
		_ENV_IMAGE_ANALYSIS_JOB_MEMORY_REQUEST,
		_ENV_IMAGE_ANALYSIS_JOB_MEMORY_LIMIT,
		_ENV_IMAGE_ANALYSIS_WORKSPACE_SIZE_LIMIT,
		_ENV_IMAGE_ANALYSIS_REGISTRY_PULL_RATE_PER_MINUTE,
		_ENV_IMAGE_ANALYSIS_JOB_TOLERATIONS,
		_ENV_IMAGE_ANALYSIS_PREFER_LOCAL,
		_ENV_IMAGE_ANALYSIS_CONTAINERD_SOCKET_PATH,
		_ENV_IMAGE_ANALYSIS_CONTAINERD_NAMESPACE,
		_ENV_IMAGE_ANALYSIS_FALLBACK_TO_REMOTE,
		_ENV_IMAGE_ANALYSIS_REMOTE_PULL_TIMEOUT,
		_ENV_IMAGE_ANALYSIS_REGISTRY_AUTH_SECRET,
		_ENV_IMAGE_ANALYSIS_TARGET_NAMESPACES,
		_ENV_IMAGE_ANALYSIS_EXCLUDED_NAMESPACES,
		_ENV_IMAGE_ANALYSIS_EXCLUDED_IMAGES,
		_ENV_IMAGE_ANALYSIS_INCLUDED_IMAGES,
		_ENV_IMAGE_ANALYSIS_LOWEST_EFFICIENCY,
		_ENV_IMAGE_ANALYSIS_HIGHEST_WASTED_BYTES,
		_ENV_IMAGE_ANALYSIS_HIGHEST_USER_WASTED_PERCENT,
		_ENV_IMAGE_ANALYSIS_RESULT_RETENTION_DAYS,
	}
	for _, v := range envVars {
		os.Unsetenv(v)
	}
}

func TestDefaultImageAnalysisConfig(t *testing.T) {
	cfg := DefaultImageAnalysisConfig()

	assert.True(t, cfg.Enabled)
	assert.Equal(t, 7, cfg.IntervalDays)
	assert.Equal(t, "", cfg.CronExpression)
	assert.Equal(t, 10, cfg.MaxConcurrentJobs)
	assert.Equal(t, 1, cfg.MaxJobsPerNode)
	assert.Equal(t, 10, cfg.BatchSize)
	assert.Equal(t, 30, cfg.JobTimeoutMinutes)
	assert.Equal(t, 2, cfg.MaxRetries)
	assert.Equal(t, "devzero-zxporter", cfg.JobNamespace)
	assert.Equal(t, "devzeroinc/image-analyzer:v1", cfg.AnalyzerImage)
	assert.Equal(t, "500m", cfg.JobCPURequest)
	assert.Equal(t, "1", cfg.JobCPULimit)
	assert.Equal(t, "1Gi", cfg.JobMemoryRequest)
	assert.Equal(t, "4Gi", cfg.JobMemoryLimit)
	assert.Equal(t, "10Gi", cfg.WorkspaceSizeLimit)
	assert.Equal(t, 30, cfg.RegistryPullRatePerMinute)
	assert.Nil(t, cfg.JobTolerations)
	assert.True(t, cfg.PreferLocal)
	assert.Equal(t, "/run/containerd/containerd.sock", cfg.ContainerdSocketPath)
	assert.Equal(t, "k8s.io", cfg.ContainerdNamespace)
	assert.True(t, cfg.FallbackToRemote)
	assert.Equal(t, "5m", cfg.RemotePullTimeout)
	assert.Equal(t, "", cfg.RegistryAuthSecret)
	assert.Nil(t, cfg.TargetNamespaces)
	assert.Equal(t, []string{"kube-system", "kube-public"}, cfg.ExcludedNamespaces)
	assert.Equal(t, []string{"registry.k8s.io/pause:*"}, cfg.ExcludedImages)
	assert.Nil(t, cfg.IncludedImages)
	assert.Equal(t, 0.90, cfg.LowestEfficiency)
	assert.Equal(t, "50MB", cfg.HighestWastedBytes)
	assert.Equal(t, 0.20, cfg.HighestUserWastedPercent)
	assert.Equal(t, 30, cfg.ResultRetentionDays)
}

func TestLoadImageAnalysisConfigFromEnv_Defaults(t *testing.T) {
	clearImageAnalysisEnvVars(t)

	cfg, err := LoadImageAnalysisConfigFromEnv()
	require.NoError(t, err)

	expected := DefaultImageAnalysisConfig()
	assert.Equal(t, expected.Enabled, cfg.Enabled)
	assert.Equal(t, expected.IntervalDays, cfg.IntervalDays)
	assert.Equal(t, expected.BatchSize, cfg.BatchSize)
	assert.Equal(t, expected.JobNamespace, cfg.JobNamespace)
	assert.Equal(t, expected.ExcludedNamespaces, cfg.ExcludedNamespaces)
	assert.Equal(t, expected.LowestEfficiency, cfg.LowestEfficiency)
}

func TestLoadImageAnalysisConfigFromEnv_CustomValues(t *testing.T) {
	clearImageAnalysisEnvVars(t)

	t.Setenv(_ENV_IMAGE_ANALYSIS_ENABLED, "false")
	t.Setenv(_ENV_IMAGE_ANALYSIS_INTERVAL_DAYS, "14")
	t.Setenv(_ENV_IMAGE_ANALYSIS_CRON_EXPRESSION, "0 2 * * 0")
	t.Setenv(_ENV_IMAGE_ANALYSIS_MAX_CONCURRENT_JOBS, "20")
	t.Setenv(_ENV_IMAGE_ANALYSIS_MAX_JOBS_PER_NODE, "2")
	t.Setenv(_ENV_IMAGE_ANALYSIS_BATCH_SIZE, "5")
	t.Setenv(_ENV_IMAGE_ANALYSIS_JOB_TIMEOUT_MINUTES, "60")
	t.Setenv(_ENV_IMAGE_ANALYSIS_MAX_RETRIES, "3")
	t.Setenv(_ENV_IMAGE_ANALYSIS_JOB_NAMESPACE, "custom-ns")
	t.Setenv(_ENV_IMAGE_ANALYSIS_ANALYZER_IMAGE, "myregistry/analyzer:v2")
	t.Setenv(_ENV_IMAGE_ANALYSIS_JOB_CPU_REQUEST, "250m")
	t.Setenv(_ENV_IMAGE_ANALYSIS_JOB_CPU_LIMIT, "2")
	t.Setenv(_ENV_IMAGE_ANALYSIS_JOB_MEMORY_REQUEST, "512Mi")
	t.Setenv(_ENV_IMAGE_ANALYSIS_JOB_MEMORY_LIMIT, "2Gi")
	t.Setenv(_ENV_IMAGE_ANALYSIS_WORKSPACE_SIZE_LIMIT, "20Gi")
	t.Setenv(_ENV_IMAGE_ANALYSIS_REGISTRY_PULL_RATE_PER_MINUTE, "60")
	t.Setenv(_ENV_IMAGE_ANALYSIS_PREFER_LOCAL, "false")
	t.Setenv(_ENV_IMAGE_ANALYSIS_CONTAINERD_SOCKET_PATH, "/custom/containerd.sock")
	t.Setenv(_ENV_IMAGE_ANALYSIS_CONTAINERD_NAMESPACE, "custom.ns")
	t.Setenv(_ENV_IMAGE_ANALYSIS_FALLBACK_TO_REMOTE, "false")
	t.Setenv(_ENV_IMAGE_ANALYSIS_REMOTE_PULL_TIMEOUT, "10m")
	t.Setenv(_ENV_IMAGE_ANALYSIS_REGISTRY_AUTH_SECRET, "my-registry-secret")
	t.Setenv(_ENV_IMAGE_ANALYSIS_TARGET_NAMESPACES, "ns1, ns2, ns3")
	t.Setenv(_ENV_IMAGE_ANALYSIS_EXCLUDED_NAMESPACES, "test-ns")
	t.Setenv(_ENV_IMAGE_ANALYSIS_EXCLUDED_IMAGES, "pause:*,debug:*")
	t.Setenv(_ENV_IMAGE_ANALYSIS_INCLUDED_IMAGES, "nginx:*,redis:*")
	t.Setenv(_ENV_IMAGE_ANALYSIS_LOWEST_EFFICIENCY, "0.95")
	t.Setenv(_ENV_IMAGE_ANALYSIS_HIGHEST_WASTED_BYTES, "100MB")
	t.Setenv(_ENV_IMAGE_ANALYSIS_HIGHEST_USER_WASTED_PERCENT, "0.10")
	t.Setenv(_ENV_IMAGE_ANALYSIS_RESULT_RETENTION_DAYS, "60")

	cfg, err := LoadImageAnalysisConfigFromEnv()
	require.NoError(t, err)

	assert.False(t, cfg.Enabled)
	assert.Equal(t, 14, cfg.IntervalDays)
	assert.Equal(t, "0 2 * * 0", cfg.CronExpression)
	assert.Equal(t, 20, cfg.MaxConcurrentJobs)
	assert.Equal(t, 2, cfg.MaxJobsPerNode)
	assert.Equal(t, 5, cfg.BatchSize)
	assert.Equal(t, 60, cfg.JobTimeoutMinutes)
	assert.Equal(t, 3, cfg.MaxRetries)
	assert.Equal(t, "custom-ns", cfg.JobNamespace)
	assert.Equal(t, "myregistry/analyzer:v2", cfg.AnalyzerImage)
	assert.Equal(t, "250m", cfg.JobCPURequest)
	assert.Equal(t, "2", cfg.JobCPULimit)
	assert.Equal(t, "512Mi", cfg.JobMemoryRequest)
	assert.Equal(t, "2Gi", cfg.JobMemoryLimit)
	assert.Equal(t, "20Gi", cfg.WorkspaceSizeLimit)
	assert.Equal(t, 60, cfg.RegistryPullRatePerMinute)
	assert.False(t, cfg.PreferLocal)
	assert.Equal(t, "/custom/containerd.sock", cfg.ContainerdSocketPath)
	assert.Equal(t, "custom.ns", cfg.ContainerdNamespace)
	assert.False(t, cfg.FallbackToRemote)
	assert.Equal(t, "10m", cfg.RemotePullTimeout)
	assert.Equal(t, "my-registry-secret", cfg.RegistryAuthSecret)
	assert.Equal(t, []string{"ns1", "ns2", "ns3"}, cfg.TargetNamespaces)
	assert.Equal(t, []string{"test-ns"}, cfg.ExcludedNamespaces)
	assert.Equal(t, []string{"pause:*", "debug:*"}, cfg.ExcludedImages)
	assert.Equal(t, []string{"nginx:*", "redis:*"}, cfg.IncludedImages)
	assert.Equal(t, 0.95, cfg.LowestEfficiency)
	assert.Equal(t, "100MB", cfg.HighestWastedBytes)
	assert.Equal(t, 0.10, cfg.HighestUserWastedPercent)
	assert.Equal(t, 60, cfg.ResultRetentionDays)
}

func TestLoadImageAnalysisConfigFromEnv_Tolerations(t *testing.T) {
	clearImageAnalysisEnvVars(t)

	t.Setenv(_ENV_IMAGE_ANALYSIS_JOB_TOLERATIONS, `[{"key":"dedicated","operator":"Exists","effect":"NoSchedule"}]`)

	cfg, err := LoadImageAnalysisConfigFromEnv()
	require.NoError(t, err)
	require.Len(t, cfg.JobTolerations, 1)
	assert.Equal(t, "dedicated", cfg.JobTolerations[0].Key)
	assert.Equal(t, "Exists", string(cfg.JobTolerations[0].Operator))
	assert.Equal(t, "NoSchedule", string(cfg.JobTolerations[0].Effect))
}

func TestLoadImageAnalysisConfigFromEnv_InvalidInt(t *testing.T) {
	clearImageAnalysisEnvVars(t)

	t.Setenv(_ENV_IMAGE_ANALYSIS_INTERVAL_DAYS, "not-a-number")

	_, err := LoadImageAnalysisConfigFromEnv()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid integer for IMAGE_ANALYSIS_INTERVAL_DAYS")
}

func TestLoadImageAnalysisConfigFromEnv_InvalidBool(t *testing.T) {
	clearImageAnalysisEnvVars(t)

	t.Setenv(_ENV_IMAGE_ANALYSIS_ENABLED, "not-a-bool")

	_, err := LoadImageAnalysisConfigFromEnv()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid boolean for IMAGE_ANALYSIS_ENABLED")
}

func TestLoadImageAnalysisConfigFromEnv_InvalidFloat(t *testing.T) {
	clearImageAnalysisEnvVars(t)

	t.Setenv(_ENV_IMAGE_ANALYSIS_LOWEST_EFFICIENCY, "abc")

	_, err := LoadImageAnalysisConfigFromEnv()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid float for IMAGE_ANALYSIS_LOWEST_EFFICIENCY")
}

func TestLoadImageAnalysisConfigFromEnv_InvalidTolerationJSON(t *testing.T) {
	clearImageAnalysisEnvVars(t)

	t.Setenv(_ENV_IMAGE_ANALYSIS_JOB_TOLERATIONS, "not-json")

	_, err := LoadImageAnalysisConfigFromEnv()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid JSON for IMAGE_ANALYSIS_JOB_TOLERATIONS")
}

func TestLoadImageAnalysisConfigFromEnv_EmptyTolerationsArray(t *testing.T) {
	clearImageAnalysisEnvVars(t)

	t.Setenv(_ENV_IMAGE_ANALYSIS_JOB_TOLERATIONS, "[]")

	cfg, err := LoadImageAnalysisConfigFromEnv()
	require.NoError(t, err)
	assert.Empty(t, cfg.JobTolerations)
}

func TestLoadImageAnalysisConfigFromEnv_CSVTrimming(t *testing.T) {
	clearImageAnalysisEnvVars(t)

	t.Setenv(_ENV_IMAGE_ANALYSIS_TARGET_NAMESPACES, " ns1 , ns2 , ns3 ")

	cfg, err := LoadImageAnalysisConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, []string{"ns1", "ns2", "ns3"}, cfg.TargetNamespaces)
}
