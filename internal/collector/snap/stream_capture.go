package snap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/transport"
)

const (
	// metadataListPageSize bounds one page of PartialObjectMetadata objects.
	metadataListPageSize = 1000
	// podListPageSize bounds one page of full pod objects (pods need the
	// typed client because exclusion rules read spec.nodeName).
	podListPageSize = 500
	// snapshotBatchSize is the flush threshold for outgoing batches; peak
	// sender memory is one page of listed objects plus one batch of entries.
	snapshotBatchSize = 2000

	// snapshotStreamingDisabledEnv forces the legacy full-snapshot path; an
	// escape hatch if the streaming path misbehaves in a customer cluster.
	snapshotStreamingDisabledEnv = "SNAPSHOT_STREAMING_DISABLED"
	// streamingReprobeInterval: after an unsupported backend is detected,
	// retry the streaming path every Nth snapshot cycle (backends upgrade).
	streamingReprobeInterval = 8

	rtDeploymentWire = gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT
	rtServiceWire    = gen.ResourceType_RESOURCE_TYPE_SERVICE
)

// snapshotBatchOpener is the sender capability the streaming path needs;
// discovered on transport.DirectSender via type assertion, like the legacy
// SendClusterSnapshotStream capability.
type snapshotBatchOpener interface {
	OpenClusterSnapshotBatchStream(
		ctx context.Context,
		snapshotID string,
		timestamp time.Time,
		isFullSnapshot bool,
	) (transport.SnapshotBatchStream, error)
}

// snapshotSource emits all entries of one resource type through emit, in
// pages. Returning an error marks the type's listing as incomplete: batches
// already emitted stand, but the type never gets its type_complete marker so
// the receiver won't run its deletion diff.
type snapshotSource struct {
	rt   gen.ResourceType
	name string
	walk func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error
}

// errSnapshotSendFailed distinguishes transport failures (fatal for the whole
// snapshot) from per-type listing failures (skip the type, keep going).
var errSnapshotSendFailed = errors.New("snapshot batch send failed")

// streamSnapshotSources drives one snapshot stream: walks every source in
// order, batches its entries, marks each fully-listed type complete, and
// finishes the stream. Listing failures skip a type; send failures abort.
func streamSnapshotSources(
	ctx context.Context,
	logger logr.Logger,
	stream transport.SnapshotBatchStream,
	sources []snapshotSource,
	batchSize int,
) (*gen.SendClusterSnapshotBatchedResponse, error) {
	for _, source := range sources {
		var pending []*gen.SnapshotEntry

		emit := func(entries []*gen.SnapshotEntry) error {
			pending = append(pending, entries...)
			for len(pending) >= batchSize {
				if err := stream.SendBatch(source.rt, pending[:batchSize], false); err != nil {
					return fmt.Errorf("%w: %s: %w", errSnapshotSendFailed, source.name, err)
				}
				pending = pending[batchSize:]
			}
			return nil
		}

		if err := source.walk(ctx, emit); err != nil {
			if errors.Is(err, errSnapshotSendFailed) {
				stream.Abort()
				return nil, err
			}
			logger.Error(err, "Failed to list resource type for snapshot; leaving type incomplete",
				"type", source.name)
			continue
		}

		// Final (possibly empty) batch carries the type_complete marker:
		// "listed successfully, this is everything".
		if err := stream.SendBatch(source.rt, pending, true); err != nil {
			stream.Abort()
			return nil, fmt.Errorf("%w: %s: %w", errSnapshotSendFailed, source.name, err)
		}
	}

	resp, err := stream.Finish()
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// streamClusterState captures and sends the cluster state through the given
// stream without ever materializing the full snapshot.
func (c *ClusterSnapshotter) streamClusterState(
	ctx context.Context,
	stream transport.SnapshotBatchStream,
) (*gen.SendClusterSnapshotBatchedResponse, error) {
	return streamSnapshotSources(ctx, c.logger, stream, c.streamSources(), snapshotBatchSize)
}

// entryFilter drops entries that must not appear in the snapshot (excluded
// nodes, non-target namespaces).
type entryFilter func(namespace, name string) bool

// metadataWalk pages one GVR through the metadata client, emitting
// UID/name/namespace entries. Namespaced GVRs honor the target-namespace
// scoping by listing per namespace.
//
// Deliberately hand-rolled rather than client-go's pager.New: pager's
// FullListIfExpired defaults to true, so an expired continue token (410 Gone
// mid-walk on a busy cluster) silently falls back to a FULL unpaginated list —
// reintroducing the O(cluster) memory spike this path exists to eliminate.
// Here an expired token surfaces as an error, the type is left incomplete
// (receiver skips its deletion diff), and the next snapshot cycle retries.
func (c *ClusterSnapshotter) metadataWalk(gvr schema.GroupVersionResource, namespaced bool, filter entryFilter) func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
	return func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
		namespaces := []string{metav1.NamespaceAll}
		if namespaced && len(c.namespaces) > 0 && c.namespaces[0] != "" {
			namespaces = c.namespaces
		}

		for _, ns := range namespaces {
			continueToken := ""
			for {
				opts := metav1.ListOptions{Limit: metadataListPageSize, Continue: continueToken}
				var list *metav1.PartialObjectMetadataList
				var err error
				if namespaced {
					list, err = c.metadataClient.Resource(gvr).Namespace(ns).List(ctx, opts)
				} else {
					list, err = c.metadataClient.Resource(gvr).List(ctx, opts)
				}
				if err != nil {
					return err
				}

				entries := make([]*gen.SnapshotEntry, 0, len(list.Items))
				for i := range list.Items {
					item := &list.Items[i]
					if filter != nil && !filter(item.Namespace, item.Name) {
						continue
					}
					entries = append(entries, &gen.SnapshotEntry{
						Uid:       string(item.UID),
						Name:      item.Name,
						Namespace: item.Namespace,
					})
				}
				if err := emit(entries); err != nil {
					return err
				}

				continueToken = list.Continue
				if continueToken == "" {
					break
				}
			}
		}
		return nil
	}
}

// podWalk pages pods through the typed client (exclusion rules need
// spec.nodeName) and emits scheduled and unscheduled pods as one flat type.
func (c *ClusterSnapshotter) podWalk() func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
	return func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
		namespaces := []string{metav1.NamespaceAll}
		if len(c.namespaces) > 0 && c.namespaces[0] != "" {
			namespaces = c.namespaces
		}

		for _, ns := range namespaces {
			continueToken := ""
			for {
				pods, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
					Limit:    podListPageSize,
					Continue: continueToken,
				})
				if err != nil {
					return err
				}

				entries := make([]*gen.SnapshotEntry, 0, len(pods.Items))
				for i := range pods.Items {
					pod := &pods.Items[i]
					if c.isPodExcluded(pod) {
						continue
					}
					// Pods on excluded nodes are excluded from the snapshot,
					// matching the legacy node-grouped capture.
					if pod.Spec.NodeName != "" && c.excludedNodes[pod.Spec.NodeName] {
						continue
					}
					entries = append(entries, &gen.SnapshotEntry{
						Uid:       string(pod.UID),
						Name:      pod.Name,
						Namespace: pod.Namespace,
					})
				}
				if err := emit(entries); err != nil {
					return err
				}

				continueToken = pods.Continue
				if continueToken == "" {
					break
				}
			}
		}
		return nil
	}
}

// optionalWalk wraps a walk for types backed by CRDs that may not be
// installed (KEDA, CNPG): a definitive "kind does not exist" is an empty,
// successfully-listed type rather than a listing failure.
func optionalWalk(walk func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error) func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
	return func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
		err := walk(ctx, emit)
		if err != nil && (apierrors.IsNotFound(err) || meta.IsNoMatchError(err)) {
			return nil
		}
		return err
	}
}

// uncollectedWireTypes are wire resource types this agent has no collector
// for; their slots in the collector enum exist only to keep the numbering
// stable (see collector.Secret, collector.ConfigMap). Listing them in
// streamSources is always a mistake, for two reasons that hold for every type
// here:
//
//  1. It cannot converge anything. dakr diffs a type only against rows this
//     agent sent, and no collector ever sends these, so the snapshot can only
//     report the cluster's entire inventory as "missing" — and
//     missingResourceHandlerKey has no handler to refresh it with.
//  2. It starves real work. dakr caps the missing-resources response globally
//     across all types, so thousands of unrefreshable entries (Helm keeps one
//     Secret per release revision) would crowd out the types that do have
//     refresh handlers and appear later in the source order.
//
// Secrets additionally cannot be listed at all: the manager ClusterRole grants
// only resourceName-scoped access to the agent's own token Secret, by design.
// A list verb cannot be narrowed to metadata — PartialObjectMetadata is a
// response projection applied after authorization, so listing secret metadata
// demands the same permission as reading every secret value in the cluster.
var uncollectedWireTypes = map[gen.ResourceType]string{
	gen.ResourceType_RESOURCE_TYPE_SECRET:     "secrets",
	gen.ResourceType_RESOURCE_TYPE_CONFIG_MAP: "configmaps",
}

// streamSources builds the ordered resource-type table. Owners come before
// dependents (namespaces, nodes, workload owners, replicasets/jobs, pods)
// so the receiver's missing-resources response is naturally parent-first,
// which keeps the agent's refresh from racing owner resolution in dakr.
func (c *ClusterSnapshotter) streamSources() []snapshotSource {
	core := func(resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Version: "v1", Resource: resource}
	}
	apps := func(resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: resource}
	}

	targetNamespaces := map[string]bool{}
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		for _, ns := range c.namespaces {
			targetNamespaces[ns] = true
		}
	}
	namespaceFilter := entryFilter(nil)
	if len(targetNamespaces) > 0 {
		namespaceFilter = func(_, name string) bool { return targetNamespaces[name] }
	}
	nodeFilter := func(_, name string) bool { return !c.excludedNodes[name] }

	return []snapshotSource{
		{rt: gen.ResourceType_RESOURCE_TYPE_NAMESPACE, name: "namespaces", walk: c.metadataWalk(core("namespaces"), false, namespaceFilter)},
		{rt: gen.ResourceType_RESOURCE_TYPE_NODE, name: "nodes", walk: c.metadataWalk(core("nodes"), false, nodeFilter)},

		// Workload owners before their dependents.
		{rt: gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT, name: "deployments", walk: c.metadataWalk(apps("deployments"), true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_STATEFUL_SET, name: "statefulsets", walk: c.metadataWalk(apps("statefulsets"), true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_DAEMON_SET, name: "daemonsets", walk: c.metadataWalk(apps("daemonsets"), true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_CRON_JOB, name: "cronjobs", walk: c.metadataWalk(schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_REPLICA_SET, name: "replicasets", walk: c.metadataWalk(apps("replicasets"), true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_JOB, name: "jobs", walk: c.metadataWalk(schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_POD, name: "pods", walk: c.podWalk()},

		// Remaining namespaced types.
		{rt: gen.ResourceType_RESOURCE_TYPE_SERVICE, name: "services", walk: c.metadataWalk(core("services"), true, nil)},
		// Secrets are deliberately absent here: see uncollectedWireTypes.
		{rt: gen.ResourceType_RESOURCE_TYPE_PERSISTENT_VOLUME_CLAIM, name: "persistentvolumeclaims", walk: c.metadataWalk(core("persistentvolumeclaims"), true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_INGRESS, name: "ingresses", walk: c.metadataWalk(schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_NETWORK_POLICY, name: "networkpolicies", walk: c.metadataWalk(schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}, true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_SERVICE_ACCOUNT, name: "serviceaccounts", walk: c.metadataWalk(core("serviceaccounts"), true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_ROLE, name: "roles", walk: c.metadataWalk(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_ROLE_BINDING, name: "rolebindings", walk: c.metadataWalk(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_POD_DISRUPTION_BUDGET, name: "poddisruptionbudgets", walk: c.metadataWalk(schema.GroupVersionResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"}, true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_ENDPOINTS, name: "endpoints", walk: c.metadataWalk(core("endpoints"), true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_LIMIT_RANGE, name: "limitranges", walk: c.metadataWalk(core("limitranges"), true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_RESOURCE_QUOTA, name: "resourcequotas", walk: c.metadataWalk(core("resourcequotas"), true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_HORIZONTAL_POD_AUTOSCALER, name: "horizontalpodautoscalers", walk: c.metadataWalk(schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}, true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_KEDA_SCALED_JOB, name: "kedascaledjobs", walk: optionalWalk(c.metadataWalk(schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledjobs"}, true, nil))},
		{rt: gen.ResourceType_RESOURCE_TYPE_KEDA_SCALED_OBJECT, name: "kedascaledobjects", walk: optionalWalk(c.metadataWalk(schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}, true, nil))},
		{rt: gen.ResourceType_RESOURCE_TYPE_CSI_STORAGE_CAPACITY, name: "csistoragecapacities", walk: c.metadataWalk(schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "csistoragecapacities"}, true, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_CNPG_CLUSTER, name: "cnpgclusters", walk: optionalWalk(c.metadataWalk(schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}, true, nil))},

		// Cluster-scoped types.
		{rt: gen.ResourceType_RESOURCE_TYPE_PERSISTENT_VOLUME, name: "persistentvolumes", walk: c.metadataWalk(core("persistentvolumes"), false, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_STORAGE_CLASS, name: "storageclasses", walk: c.metadataWalk(schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}, false, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_CLUSTER_ROLE, name: "clusterroles", walk: c.metadataWalk(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, false, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_CLUSTER_ROLE_BINDING, name: "clusterrolebindings", walk: c.metadataWalk(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, false, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_INGRESS_CLASS, name: "ingressclasses", walk: c.metadataWalk(schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingressclasses"}, false, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_CSI_NODE, name: "csinodes", walk: c.metadataWalk(schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "csinodes"}, false, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_CSI_DRIVER, name: "csidrivers", walk: c.metadataWalk(schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "csidrivers"}, false, nil)},
		{rt: gen.ResourceType_RESOURCE_TYPE_VOLUME_ATTACHMENT, name: "volumeattachments", walk: c.metadataWalk(schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "volumeattachments"}, false, nil)},
	}
}

// streamingSupported decides whether this cycle uses the streaming path.
func (c *ClusterSnapshotter) streamingSupported() (snapshotBatchOpener, bool) {
	if os.Getenv(snapshotStreamingDisabledEnv) == "true" {
		return nil, false
	}
	if c.metadataClient == nil {
		return nil, false
	}
	opener, ok := c.sender.(snapshotBatchOpener)
	if !ok {
		return nil, false
	}

	c.mu.RLock()
	legacyOnly := c.streamingUnsupported
	cycle := c.snapshotCycle
	c.mu.RUnlock()

	// Re-probe a previously-unsupported backend occasionally: it upgrades
	// independently of the agent.
	if legacyOnly && cycle%streamingReprobeInterval != 0 {
		return nil, false
	}
	return opener, true
}

// streamSnapshot runs one full streaming snapshot cycle.
func (c *ClusterSnapshotter) streamSnapshot(ctx context.Context, opener snapshotBatchOpener) error {
	now := time.Now().UTC()
	clusterID := ""
	if v, ok := ctx.Value("cluster_id").(string); ok {
		clusterID = v
	}
	snapshotID := fmt.Sprintf("snapshot-unknown-%d", now.UnixNano())
	if clusterID != "" {
		snapshotID = fmt.Sprintf("snapshot-%s-%d", clusterID, now.UnixNano())
	}

	stream, err := opener.OpenClusterSnapshotBatchStream(ctx, snapshotID, now, true)
	if err != nil {
		return err
	}

	resp, err := c.streamClusterState(ctx, stream)
	if err != nil {
		return err
	}

	c.logger.Info("Successfully streamed cluster snapshot",
		"snapshotId", snapshotID,
		"batches", resp.GetBatchesReceived(),
		"status", resp.GetStatus(),
		"missingResources", len(resp.GetMissingResources()),
		"missingTruncated", resp.GetMissingResourcesTruncated())

	if len(resp.GetMissingResources()) > 0 {
		if refreshErr := c.refreshMissingList(ctx, resp.GetMissingResources()); refreshErr != nil {
			c.logger.Error(refreshErr, "Failed to refresh missing resources from batched snapshot response")
		}
	}
	return nil
}
