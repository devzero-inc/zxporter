package snap

import (
	"context"
	"fmt"
	"sort"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// missingRefreshRank orders missing-resource refreshes parents-first so that
// owners (Deployments, CronJobs, ...) reach dakr before their dependents
// (ReplicaSets, Jobs) and everything else; this keeps dakr's owner resolution
// from NACKing dependents whose parents haven't been ingested yet.
func missingRefreshRank(rt gen.ResourceType) int {
	switch rt {
	case gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT,
		gen.ResourceType_RESOURCE_TYPE_STATEFUL_SET,
		gen.ResourceType_RESOURCE_TYPE_DAEMON_SET,
		gen.ResourceType_RESOURCE_TYPE_CRON_JOB:
		return 0
	case gen.ResourceType_RESOURCE_TYPE_REPLICA_SET,
		gen.ResourceType_RESOURCE_TYPE_JOB:
		return 1
	default:
		return 2
	}
}

// orderMissingResources returns the missing list sorted parents-first,
// preserving the incoming order within a rank.
func orderMissingResources(missing []*gen.MissingResource) []*gen.MissingResource {
	ordered := append([]*gen.MissingResource(nil), missing...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return missingRefreshRank(ordered[i].GetResourceType()) < missingRefreshRank(ordered[j].GetResourceType())
	})
	return ordered
}

// missingResourceHandlerKey maps a wire resource type to the refresh handler
// key used by clusterHandlers/namespacedHandlers. Types without a refresh
// handler (parity with the legacy refresh maps) return ok=false and are
// skipped; they converge through their regular collectors instead.
func missingResourceHandlerKey(rt gen.ResourceType) (key string, namespaced bool, ok bool) {
	switch rt {
	case gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT:
		return "deployment", true, true
	case gen.ResourceType_RESOURCE_TYPE_STATEFUL_SET:
		return "stateful_set", true, true
	case gen.ResourceType_RESOURCE_TYPE_DAEMON_SET:
		return "daemon_set", true, true
	case gen.ResourceType_RESOURCE_TYPE_REPLICA_SET:
		return "replica_set", true, true
	case gen.ResourceType_RESOURCE_TYPE_SERVICE:
		return "service", true, true
	case gen.ResourceType_RESOURCE_TYPE_PERSISTENT_VOLUME_CLAIM:
		return "persistent_volume_claim", true, true
	case gen.ResourceType_RESOURCE_TYPE_JOB:
		return "job", true, true
	case gen.ResourceType_RESOURCE_TYPE_CRON_JOB:
		return "cron_job", true, true
	case gen.ResourceType_RESOURCE_TYPE_INGRESS:
		return "ingress", true, true
	case gen.ResourceType_RESOURCE_TYPE_NETWORK_POLICY:
		return "network_policy", true, true
	case gen.ResourceType_RESOURCE_TYPE_SERVICE_ACCOUNT:
		return "service_account", true, true
	case gen.ResourceType_RESOURCE_TYPE_ROLE:
		return "role", true, true
	case gen.ResourceType_RESOURCE_TYPE_ROLE_BINDING:
		return "role_binding", true, true
	case gen.ResourceType_RESOURCE_TYPE_ENDPOINTS:
		return "endpoints", true, true
	case gen.ResourceType_RESOURCE_TYPE_HORIZONTAL_POD_AUTOSCALER:
		return "horizontal_pod_autoscaler", true, true
	case gen.ResourceType_RESOURCE_TYPE_PERSISTENT_VOLUME:
		return "persistent_volume", false, true
	case gen.ResourceType_RESOURCE_TYPE_STORAGE_CLASS:
		return "storage_class", false, true
	case gen.ResourceType_RESOURCE_TYPE_CLUSTER_ROLE:
		return "cluster_role", false, true
	case gen.ResourceType_RESOURCE_TYPE_CLUSTER_ROLE_BINDING:
		return "cluster_role_binding", false, true
	case gen.ResourceType_RESOURCE_TYPE_INGRESS_CLASS:
		return "ingress_class", false, true
	case gen.ResourceType_RESOURCE_TYPE_CSI_NODE:
		return "csi_node", false, true
	case gen.ResourceType_RESOURCE_TYPE_CSI_DRIVER:
		return "csi_driver", false, true
	case gen.ResourceType_RESOURCE_TYPE_VOLUME_ATTACHMENT:
		return "volume_attachment", false, true
	default:
		return "", false, false
	}
}

// refreshMissingList re-fetches resources the backend reported missing from
// the batched snapshot response and pushes them through their collectors,
// parents before dependents.
func (c *ClusterSnapshotter) refreshMissingList(
	ctx context.Context,
	missing []*gen.MissingResource,
) error {
	if c.collectorManager == nil {
		return fmt.Errorf("collector manager not available")
	}

	c.logger.Info("Refreshing missing resources from batched snapshot response", "count", len(missing))

	for _, m := range orderMissingResources(missing) {
		key, namespaced, ok := missingResourceHandlerKey(m.GetResourceType())
		if !ok {
			c.logger.Info("No refresh handler for missing resource type; skipping",
				"type", m.GetResourceType().String(),
				"name", m.GetName())
			continue
		}

		var err error
		if namespaced {
			err = c.refreshNamespacedResource(ctx, key, m.GetNamespace(), m.GetUid(), m.GetName())
		} else {
			err = c.refreshResource(ctx, key, m.GetUid(), m.GetName())
		}
		if err != nil {
			c.logger.Error(err, "Failed to refresh missing resource",
				"type", key,
				"name", m.GetName(),
				"namespace", m.GetNamespace())
		}
	}

	c.logger.Info("Completed refresh of missing resources")
	return nil
}
