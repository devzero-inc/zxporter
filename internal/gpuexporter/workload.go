package gpuexporter

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	lru "github.com/hashicorp/golang-lru/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	KindPod         = "Pod"
	KindJob         = "Job"
	KindCronJob     = "CronJob"
	KindRollout     = "Rollout"
	KindDaemonSet   = "DaemonSet"
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindReplicaSet  = "ReplicaSet"
)

var kindToGVR = map[string]schema.GroupVersionResource{
	KindPod:         {Group: "", Version: "v1", Resource: "pods"},
	KindReplicaSet:  {Group: "apps", Version: "v1", Resource: "replicasets"},
	KindDeployment:  {Group: "apps", Version: "v1", Resource: "deployments"},
	KindStatefulSet: {Group: "apps", Version: "v1", Resource: "statefulsets"},
	KindDaemonSet:   {Group: "apps", Version: "v1", Resource: "daemonsets"},
	KindJob:         {Group: "batch", Version: "v1", Resource: "jobs"},
	KindCronJob:     {Group: "batch", Version: "v1", Resource: "cronjobs"},
	KindRollout:     {Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"},
}

// Workload represents a resolved Kubernetes workload.
type Workload struct {
	Name      string
	Namespace string
	Kind      string
}

type cacheKey struct {
	namespace string
	name      string
}

// WorkloadResolverConfig holds configuration for the workload resolver.
type WorkloadResolverConfig struct {
	LabelKeys []string
	CacheSize int
}

type workloadResolver struct {
	dynClient dynamic.Interface
	lru       *lru.Cache[cacheKey, *Workload]
	labelKeys []string
	log       logr.Logger
}

// NewWorkloadResolver creates a WorkloadResolver that uses the K8s dynamic client
// to walk owner references and find the top-level owning workload.
// Supports LRU caching and label-based workload name resolution.
func NewWorkloadResolver(
	dynClient dynamic.Interface,
	cfg WorkloadResolverConfig,
	log logr.Logger,
) WorkloadResolver {
	cacheSize := cfg.CacheSize
	if cacheSize <= 0 {
		cacheSize = 256
	}
	cache, _ := lru.New[cacheKey, *Workload](cacheSize)

	return &workloadResolver{
		dynClient: dynClient,
		lru:       cache,
		labelKeys: cfg.LabelKeys,
		log:       log.WithName("workload-resolver"),
	}
}

func (r *workloadResolver) FindWorkloadForPod(
	ctx context.Context,
	name, namespace string,
) (string, string, error) {
	key := cacheKey{namespace: namespace, name: name}
	if w, ok := r.lru.Get(key); ok {
		return w.Kind, w.Name, nil
	}

	pod, err := r.dynClient.Resource(kindToGVR[KindPod]).
		Namespace(namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("getting pod %s/%s: %w", namespace, name, err)
	}

	// Label-based resolution: check pod labels for workload name
	if workloadName, ok := r.findWorkloadNameFromLabels(pod.GetLabels()); ok {
		kind := r.findTopControllerKind(ctx, pod)
		w := &Workload{Name: workloadName, Namespace: namespace, Kind: kind}
		r.lru.Add(key, w)
		return w.Kind, w.Name, nil
	}

	// Owner-reference-based resolution
	w, err := r.findPodOwner(ctx, pod)
	if err != nil {
		w = &Workload{Name: pod.GetName(), Namespace: namespace, Kind: KindPod}
	}

	r.lru.Add(key, w)
	return w.Kind, w.Name, nil
}

func (r *workloadResolver) findWorkloadNameFromLabels(labels map[string]string) (string, bool) {
	if len(r.labelKeys) == 0 {
		return "", false
	}
	for _, key := range r.labelKeys {
		if val, ok := labels[key]; ok {
			return val, true
		}
	}
	return "", false
}

func (r *workloadResolver) findTopControllerKind(ctx context.Context, obj metav1.Object) string {
	ownerRef := metav1.GetControllerOfNoCopy(obj)
	if ownerRef == nil {
		return KindPod
	}

	kind := ownerRef.Kind
	name := ownerRef.Name
	namespace := obj.GetNamespace()

	for {
		gvr, ok := kindToGVR[kind]
		if !ok {
			return kind
		}

		next, err := r.dynClient.Resource(gvr).
			Namespace(namespace).
			Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return kind
		}

		nextOwner := metav1.GetControllerOfNoCopy(next)
		if nextOwner == nil {
			return kind
		}

		kind = nextOwner.Kind
		name = nextOwner.Name
	}
}

func (r *workloadResolver) findPodOwner(ctx context.Context, pod metav1.Object) (*Workload, error) {
	ownerRef := metav1.GetControllerOfNoCopy(pod)
	if ownerRef == nil {
		return &Workload{
			Name:      pod.GetName(),
			Namespace: pod.GetNamespace(),
			Kind:      KindPod,
		}, nil
	}

	namespace := pod.GetNamespace()

	switch ownerRef.Kind {
	case KindReplicaSet:
		rs, err := r.dynClient.Resource(kindToGVR[KindReplicaSet]).
			Namespace(namespace).
			Get(ctx, ownerRef.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting replicaset %s/%s: %w", namespace, ownerRef.Name, err)
		}

		if rsOwner := metav1.GetControllerOfNoCopy(rs); rsOwner != nil &&
			(rsOwner.Kind == KindDeployment || rsOwner.Kind == KindRollout) {
			return &Workload{
				Name:      rsOwner.Name,
				Namespace: namespace,
				Kind:      rsOwner.Kind,
			}, nil
		}

		return &Workload{
			Name:      ownerRef.Name,
			Namespace: namespace,
			Kind:      KindReplicaSet,
		}, nil

	case KindJob:
		job, err := r.dynClient.Resource(kindToGVR[KindJob]).
			Namespace(namespace).
			Get(ctx, ownerRef.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting job %s/%s: %w", namespace, ownerRef.Name, err)
		}
		if jobOwner := metav1.GetControllerOfNoCopy(job); jobOwner != nil &&
			jobOwner.Kind == KindCronJob {
			return &Workload{
				Name:      jobOwner.Name,
				Namespace: namespace,
				Kind:      KindCronJob,
			}, nil
		}
		return &Workload{
			Name:      ownerRef.Name,
			Namespace: namespace,
			Kind:      KindJob,
		}, nil

	default:
		return &Workload{
			Name:      ownerRef.Name,
			Namespace: namespace,
			Kind:      ownerRef.Kind,
		}, nil
	}
}
