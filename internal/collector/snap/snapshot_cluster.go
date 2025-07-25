package snap

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *ClusterSnapshotter) captureClusterInfo(ctx context.Context, snapshot *ClusterSnapshot) error {
	version, err := c.client.Discovery().ServerVersion()
	if err != nil {
		c.logger.Error(err, "Failed to get server version")
		snapshot.ClusterInfo.Version = "unknown"
	} else {
		snapshot.ClusterInfo.Version = version.String()
	}

	namespaces, err := c.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list namespaces: %w", err)
	}

	var nsNames []string
	for _, ns := range namespaces.Items {
		nsNames = append(nsNames, ns.Name)
	}
	snapshot.ClusterInfo.Namespaces = nsNames

	return nil
}

func (c *ClusterSnapshotter) captureClusterScopedResources(ctx context.Context, snapshot *ClusterSnapshot) error {
	clusterScoped := snapshot.ClusterScoped
	clusterScoped.PersistentVolumes = make(map[string]ResourceIdentifier)
	clusterScoped.StorageClasses = make(map[string]ResourceIdentifier)
	clusterScoped.ClusterRoles = make(map[string]ResourceIdentifier)
	clusterScoped.ClusterRoleBindings = make(map[string]ResourceIdentifier)

	if pvs, err := c.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{}); err == nil {
		for _, pv := range pvs.Items {
			uid := string(pv.UID)
			clusterScoped.PersistentVolumes[uid] = ResourceIdentifier{Name: pv.Name}
		}
	}

	if scs, err := c.client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{}); err == nil {
		for _, sc := range scs.Items {
			uid := string(sc.UID)
			clusterScoped.StorageClasses[uid] = ResourceIdentifier{Name: sc.Name}
		}
	}

	if clusterRoles, err := c.client.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{}); err == nil {
		for _, cr := range clusterRoles.Items {
			uid := string(cr.UID)
			clusterScoped.ClusterRoles[uid] = ResourceIdentifier{Name: cr.Name}
		}
	}

	if clusterRoleBindings, err := c.client.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{}); err == nil {
		for _, crb := range clusterRoleBindings.Items {
			uid := string(crb.UID)
			clusterScoped.ClusterRoleBindings[uid] = ResourceIdentifier{Name: crb.Name}
		}
	}

	clusterScoped.Hash = c.calculateClusterScopedHash(clusterScoped)
	return nil
}

func (c *ClusterSnapshotter) calculateClusterScopedHash(clusterScoped *ClusterScopedSnapshot) string {
	h := sha256.New()
	if csBytes, err := json.Marshal(clusterScoped); err == nil {
		h.Write(csBytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
