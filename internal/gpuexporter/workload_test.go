package gpuexporter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakedynamic "k8s.io/client-go/dynamic/fake"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
)

var testGVRs = map[schema.GroupVersionResource]string{
	kindToGVR[KindPod]:         "PodList",
	kindToGVR[KindReplicaSet]:  "ReplicaSetList",
	kindToGVR[KindDeployment]:  "DeploymentList",
	kindToGVR[KindStatefulSet]: "StatefulSetList",
	kindToGVR[KindDaemonSet]:   "DaemonSetList",
	kindToGVR[KindJob]:         "JobList",
	kindToGVR[KindCronJob]:     "CronJobList",
	kindToGVR[KindRollout]:     "RolloutList",
}

func TestFindWorkloadForPod(t *testing.T) {
	ctx := context.Background()
	isController := true

	t.Run("pod with no owner returns empty", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "my-pod", "default", nil, nil)
		r := newTestResolver(t, nil, pod)

		kind, name, err := r.FindWorkloadForPod(ctx, "my-pod", "default")
		require.NoError(t, err)
		require.Equal(t, KindPod, kind)
		require.Equal(t, "my-pod", name)
	})

	t.Run("pod owned by replicaset owned by deployment", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "my-deploy-abc-xyz", "default", nil,
			[]metav1.OwnerReference{{Kind: KindReplicaSet, Name: "my-deploy-abc", Controller: &isController}},
		)
		rs := newUnstructuredObj("apps/v1", "ReplicaSet", "my-deploy-abc", "default", nil,
			[]metav1.OwnerReference{{Kind: KindDeployment, Name: "my-deploy", Controller: &isController}},
		)
		r := newTestResolver(t, nil, pod, rs)

		kind, name, err := r.FindWorkloadForPod(ctx, "my-deploy-abc-xyz", "default")
		require.NoError(t, err)
		require.Equal(t, KindDeployment, kind)
		require.Equal(t, "my-deploy", name)
	})

	t.Run("pod owned by replicaset with no deployment", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "my-rs-xyz", "default", nil,
			[]metav1.OwnerReference{{Kind: KindReplicaSet, Name: "my-rs", Controller: &isController}},
		)
		rs := newUnstructuredObj("apps/v1", "ReplicaSet", "my-rs", "default", nil, nil)
		r := newTestResolver(t, nil, pod, rs)

		kind, name, err := r.FindWorkloadForPod(ctx, "my-rs-xyz", "default")
		require.NoError(t, err)
		require.Equal(t, KindReplicaSet, kind)
		require.Equal(t, "my-rs", name)
	})

	t.Run("pod owned by replicaset owned by rollout", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "my-rollout-abc-xyz", "default", nil,
			[]metav1.OwnerReference{{Kind: KindReplicaSet, Name: "my-rollout-abc", Controller: &isController}},
		)
		rs := newUnstructuredObj("apps/v1", "ReplicaSet", "my-rollout-abc", "default", nil,
			[]metav1.OwnerReference{{Kind: KindRollout, Name: "my-rollout", Controller: &isController}},
		)
		r := newTestResolver(t, nil, pod, rs)

		kind, name, err := r.FindWorkloadForPod(ctx, "my-rollout-abc-xyz", "default")
		require.NoError(t, err)
		require.Equal(t, KindRollout, kind)
		require.Equal(t, "my-rollout", name)
	})

	t.Run("pod owned by statefulset", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "my-sts-0", "default", nil,
			[]metav1.OwnerReference{{Kind: KindStatefulSet, Name: "my-sts", Controller: &isController}},
		)
		r := newTestResolver(t, nil, pod)

		kind, name, err := r.FindWorkloadForPod(ctx, "my-sts-0", "default")
		require.NoError(t, err)
		require.Equal(t, KindStatefulSet, kind)
		require.Equal(t, "my-sts", name)
	})

	t.Run("pod owned by daemonset", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "my-ds-abc", "default", nil,
			[]metav1.OwnerReference{{Kind: KindDaemonSet, Name: "my-ds", Controller: &isController}},
		)
		r := newTestResolver(t, nil, pod)

		kind, name, err := r.FindWorkloadForPod(ctx, "my-ds-abc", "default")
		require.NoError(t, err)
		require.Equal(t, KindDaemonSet, kind)
		require.Equal(t, "my-ds", name)
	})

	t.Run("pod owned by job owned by cronjob", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "my-cron-job-abc-xyz", "default", nil,
			[]metav1.OwnerReference{{Kind: KindJob, Name: "my-cron-job-abc", Controller: &isController}},
		)
		job := newUnstructuredObj("batch/v1", "Job", "my-cron-job-abc", "default", nil,
			[]metav1.OwnerReference{{Kind: KindCronJob, Name: "my-cron", Controller: &isController}},
		)
		r := newTestResolver(t, nil, pod, job)

		kind, name, err := r.FindWorkloadForPod(ctx, "my-cron-job-abc-xyz", "default")
		require.NoError(t, err)
		require.Equal(t, KindCronJob, kind)
		require.Equal(t, "my-cron", name)
	})

	t.Run("pod owned by job with no cronjob", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "my-job-xyz", "default", nil,
			[]metav1.OwnerReference{{Kind: KindJob, Name: "my-job", Controller: &isController}},
		)
		job := newUnstructuredObj("batch/v1", "Job", "my-job", "default", nil, nil)
		r := newTestResolver(t, nil, pod, job)

		kind, name, err := r.FindWorkloadForPod(ctx, "my-job-xyz", "default")
		require.NoError(t, err)
		require.Equal(t, KindJob, kind)
		require.Equal(t, "my-job", name)
	})

	t.Run("label-based resolution uses label name and owner kind", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "my-deploy-abc-xyz", "default",
			map[string]string{"app.kubernetes.io/name": "my-app"},
			[]metav1.OwnerReference{{Kind: KindReplicaSet, Name: "my-deploy-abc", Controller: &isController}},
		)
		rs := newUnstructuredObj("apps/v1", "ReplicaSet", "my-deploy-abc", "default", nil,
			[]metav1.OwnerReference{{Kind: KindDeployment, Name: "my-deploy", Controller: &isController}},
		)
		r := newTestResolver(t, []string{"app.kubernetes.io/name"}, pod, rs)

		kind, name, err := r.FindWorkloadForPod(ctx, "my-deploy-abc-xyz", "default")
		require.NoError(t, err)
		require.Equal(t, KindDeployment, kind)
		require.Equal(t, "my-app", name)
	})

	t.Run("label-based resolution with no owner returns pod kind", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "bare-pod", "default",
			map[string]string{"app": "my-app"},
			nil,
		)
		r := newTestResolver(t, []string{"app"}, pod)

		kind, name, err := r.FindWorkloadForPod(ctx, "bare-pod", "default")
		require.NoError(t, err)
		require.Equal(t, KindPod, kind)
		require.Equal(t, "my-app", name)
	})

	t.Run("second call returns cached result", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "cached-pod", "default", nil, nil)
		r := newTestResolver(t, nil, pod)

		kind1, name1, err := r.FindWorkloadForPod(ctx, "cached-pod", "default")
		require.NoError(t, err)

		kind2, name2, err := r.FindWorkloadForPod(ctx, "cached-pod", "default")
		require.NoError(t, err)
		require.Equal(t, kind1, kind2)
		require.Equal(t, name1, name2)
	})

	t.Run("pod not found returns error", func(t *testing.T) {
		r := newTestResolver(t, nil)

		_, _, err := r.FindWorkloadForPod(ctx, "missing-pod", "default")
		require.Error(t, err)
	})

	t.Run("replicaset fetch error falls back to pod", func(t *testing.T) {
		pod := newUnstructuredObj("v1", "Pod", "orphan-pod", "default", nil,
			[]metav1.OwnerReference{{Kind: KindReplicaSet, Name: "deleted-rs", Controller: &isController}},
		)
		// ReplicaSet not in fake client — will cause fetch error
		r := newTestResolver(t, nil, pod)

		kind, name, err := r.FindWorkloadForPod(ctx, "orphan-pod", "default")
		require.NoError(t, err)
		require.Equal(t, KindPod, kind)
		require.Equal(t, "orphan-pod", name)
	})
}

func newTestResolver(t *testing.T, labelKeys []string, objects ...*unstructured.Unstructured) WorkloadResolver {
	t.Helper()

	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	scheme := runtime.NewScheme()
	var runtimeObjects []runtime.Object
	for _, obj := range objects {
		runtimeObjects = append(runtimeObjects, obj)
	}

	dynClient := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme, testGVRs, runtimeObjects...)

	return NewWorkloadResolver(dynClient, WorkloadResolverConfig{
		LabelKeys: labelKeys,
		CacheSize: 128,
	}, log)
}

func newUnstructuredObj(apiVersion, kind, name, namespace string, labels map[string]string, ownerRefs []metav1.OwnerReference) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
		},
	}

	if labels != nil {
		obj.SetLabels(labels)
	}

	if len(ownerRefs) > 0 {
		obj.SetOwnerReferences(ownerRefs)
	}

	return obj
}
