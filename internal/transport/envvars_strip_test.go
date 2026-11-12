package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
	"github.com/devzero-inc/zxporter/internal/collector"
)

func podWithEnv(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "app",
			Image: "app:1",
			Env:   []corev1.EnvVar{{Name: "DB_PASSWORD", Value: "s3cr3t"}},
		}}},
	}
}

// sentData is the object the client put on the wire. Data is deprecated in the
// proto in favour of data_bytes, but it is still the field RealDakrClient
// fills and dakr reads, so it is the one to assert on.
func sentData(item *gen.ResourceItem) map[string]any {
	return item.GetData().AsMap() //nolint:staticcheck // see sentData's doc
}

// containerEnv returns spec.containers[0].env as dakr received it, or nil.
func containerEnv(t *testing.T, data map[string]any) any {
	t.Helper()
	spec, ok := data["spec"].(map[string]any)
	require.True(t, ok, "spec missing: %v", data)
	containers, ok := spec["containers"].([]any)
	require.True(t, ok && len(containers) == 1, "containers missing: %v", spec)
	c := containers[0].(map[string]any)
	assert.Equal(t, "app:1", c["image"], "the rest of the container must still be sent")
	return c["env"]
}

func TestSendResourceBatch_StripsEnvVarsOnlyWhenTheResourceIsMarked(t *testing.T) {
	mock := &MockMetricsServer{}
	mux := http.NewServeMux()
	path, handler := apiv1connect.NewMetricsCollectorServiceHandler(mock)
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewDakrClient(server.URL, "test-token", logr.Discard())

	marked := podWithEnv("marked")
	unmarked := podWithEnv("unmarked")
	_, err := client.SendResourceBatch(context.Background(), []collector.CollectedResource{
		{ResourceType: collector.Pod, Key: "default/marked", Object: marked, StripEnvVars: true},
		{ResourceType: collector.Pod, Key: "default/unmarked", Object: unmarked},
	}, collector.Pod)
	require.NoError(t, err)

	require.Len(t, mock.receivedBatches, 1)
	items := mock.receivedBatches[0].Resources
	require.Len(t, items, 2)
	assert.Nil(t, containerEnv(t, sentData(items[0])), "marked resource must arrive without env")
	assert.Equal(t,
		[]any{map[string]any{"name": "DB_PASSWORD", "value": "s3cr3t"}},
		containerEnv(t, sentData(items[1])),
		"unmarked resource keeps today's behaviour")

	// The object handed in may be an informer-cached pointer: it must be untouched.
	require.Len(t, marked.Spec.Containers[0].Env, 1)
	assert.Equal(t, "s3cr3t", marked.Spec.Containers[0].Env[0].Value)
}
