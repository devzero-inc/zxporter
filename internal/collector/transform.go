package collector

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// lastAppliedConfigAnnotation is the annotation kubectl writes on apply. It is a
// full JSON copy of the object spec and can be several KB per object. Nothing in
// this operator reads it, so it is stripped before objects enter the informer
// cache to reduce memory in large clusters.
const lastAppliedConfigAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// StripMetadataTransform is a cache.TransformFunc applied to every informer so
// cached objects drop the two fields that dominate cache memory but are never
// read anywhere in internal/:
//
//   - metadata.managedFields — the server-side-apply bookkeeping tree (often
//     1–5 KB per object).
//   - the kubectl.kubernetes.io/last-applied-configuration annotation — a full
//     JSON duplicate of the spec.
//
// It works for both typed objects (*v1.Pod, ...) and *unstructured.Unstructured
// (used by the dynamic informers) via meta.Accessor. Objects that are not
// Kubernetes objects, and DeletedFinalStateUnknown tombstones, are returned
// unchanged. The transform mutates the object in place and returns it, which is
// the client-go contract for TransformFuncs.
func StripMetadataTransform(obj interface{}) (interface{}, error) {
	// Tombstones wrap the last known object, which was already transformed on the
	// way in — leave them alone.
	if _, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return obj, nil
	}

	accessor, err := meta.Accessor(obj)
	if err != nil {
		// Not a Kubernetes object (e.g. a *v1.Status); nothing to strip.
		return obj, nil
	}

	accessor.SetManagedFields(nil)

	if ann := accessor.GetAnnotations(); ann != nil {
		if _, ok := ann[lastAppliedConfigAnnotation]; ok {
			delete(ann, lastAppliedConfigAnnotation)
			accessor.SetAnnotations(ann)
		}
	}

	return obj, nil
}

// newInformerFactory builds a typed SharedInformerFactory with
// StripMetadataTransform always applied, optionally scoped to a single
// namespace. All collectors that use the standard client-go informers package
// should construct their factory through this helper so the memory-reduction
// transform is applied uniformly (and inherited by any future collector).
//
// Pass the collector's namespaces slice; a single non-empty namespace scopes the
// factory to it, otherwise the factory watches all namespaces. Cluster-scoped
// collectors that have no namespace concept can pass nil.
func newInformerFactory(client kubernetes.Interface, namespaces []string) informers.SharedInformerFactory {
	opts := []informers.SharedInformerOption{
		informers.WithTransform(StripMetadataTransform),
	}
	if len(namespaces) == 1 && namespaces[0] != "" {
		opts = append(opts, informers.WithNamespace(namespaces[0]))
	}
	// Resync period 0: rely on watch events, no periodic full resync.
	return informers.NewSharedInformerFactoryWithOptions(client, 0, opts...)
}
