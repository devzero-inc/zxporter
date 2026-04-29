package collector

import (
	"context"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetFilesystemUsageFromPrometheus_TransformsResponse(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-postgres-0", Namespace: "default"},
	}

	mock := &mockPrometheusAPI{
		queryResults: map[string]model.Value{
			`kubelet_volume_stats_used_bytes{namespace="default", persistentvolumeclaim="data-postgres-0"}`:      newSampleVector(1073741824.0), // 1 GiB
			`kubelet_volume_stats_capacity_bytes{namespace="default", persistentvolumeclaim="data-postgres-0"}`:  newSampleVector(10737418240.0), // 10 GiB
			`kubelet_volume_stats_available_bytes{namespace="default", persistentvolumeclaim="data-postgres-0"}`: newSampleVector(9663676416.0), // 9 GiB
		},
	}

	pc := &PersistentVolumeClaimMetricsCollector{
		prometheusAPI: mock,
	}

	usage, err := pc.getFilesystemUsageFromPrometheus(context.Background(), pvc)

	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(1073741824), usage.UsedBytes)
	assert.Equal(t, int64(10737418240), usage.CapacityBytes)
	assert.Equal(t, int64(9663676416), usage.AvailableBytes)
}

func TestGetFilesystemUsageFromPrometheus_NilAPI(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-postgres-0", Namespace: "default"},
	}

	pc := &PersistentVolumeClaimMetricsCollector{
		prometheusAPI: nil,
	}

	usage, err := pc.getFilesystemUsageFromPrometheus(context.Background(), pvc)

	assert.Error(t, err)
	assert.Nil(t, usage)
}
