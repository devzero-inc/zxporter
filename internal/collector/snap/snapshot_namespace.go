package snap

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *ClusterSnapshotter) captureNamespaces(ctx context.Context, snapshot *ClusterSnapshot) error {
	targetNamespaces := c.getTargetNamespaces(ctx)

	for _, nsName := range targetNamespaces {
		nsData := &Namespace{
			Deployments:              make(map[string]*ResourceIdentifier),
			StatefulSets:             make(map[string]*ResourceIdentifier),
			DaemonSets:               make(map[string]*ResourceIdentifier),
			ReplicaSets:              make(map[string]*ResourceIdentifier),
			Services:                 make(map[string]*ResourceIdentifier),
			Secrets:                  make(map[string]*ResourceIdentifier),
			Pvcs:                     make(map[string]*ResourceIdentifier),
			Jobs:                     make(map[string]*ResourceIdentifier),
			CronJobs:                 make(map[string]*ResourceIdentifier),
			Ingresses:                make(map[string]*ResourceIdentifier),
			NetworkPolicies:          make(map[string]*ResourceIdentifier),
			ServiceAccounts:          make(map[string]*ResourceIdentifier),
			Roles:                    make(map[string]*ResourceIdentifier),
			RoleBindings:             make(map[string]*ResourceIdentifier),
			PodDisruptionBudgets:     make(map[string]*ResourceIdentifier),
			Endpoints:                make(map[string]*ResourceIdentifier),
			LimitRanges:              make(map[string]*ResourceIdentifier),
			ResourceQuotas:           make(map[string]*ResourceIdentifier),
			UnscheduledPods:          make(map[string]*ResourceIdentifier),
			HorizontalPodAutoscalers: make(map[string]*ResourceIdentifier),
			KedaScaledJobs:           make(map[string]*ResourceIdentifier), // Not implemented
			KedaScaledObjects:        make(map[string]*ResourceIdentifier), // Not implemented
			CsiStorageCapacities:     make(map[string]*ResourceIdentifier),
		}

		ns, err := c.client.CoreV1().Namespaces().Get(ctx, nsName, metav1.GetOptions{})
		if err != nil {
			c.logger.Error(err, "Failed to get namespace", "namespace", nsName)
			continue
		}
		nsData.Namespace = &ResourceIdentifier{
			Name: ns.Name,
		}

		if err := c.captureNamespaceResources(ctx, nsName, nsData); err != nil {
			c.logger.Error(err, "Failed to capture resources for namespace", "namespace", nsName)
			continue
		}

		nsData.Hash = c.calculateNamespaceHash(nsData)
		// Use namespace UID as the map key for O(1) processor lookup
		uid := string(ns.UID)
		snapshot.Namespaces[uid] = nsData
	}

	return nil
}

func (c *ClusterSnapshotter) captureNamespaceResources(ctx context.Context, namespace string, nsData *Namespace) error {
	// Capture unscheduled pods only (scheduled pods are captured with nodes)
	pods, err := c.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range pods.Items {
		if !c.isPodExcluded(&pod) && pod.Spec.NodeName == "" {
			uid := string(pod.UID)
			nsData.UnscheduledPods[uid] = &ResourceIdentifier{
				Name: pod.Name,
			}
		}
	}

	// Capture workload controllers (without pods since pods are captured at node level)
	if deployments, err := c.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, deployment := range deployments.Items {
			uid := string(deployment.UID)
			nsData.Deployments[uid] = &ResourceIdentifier{
				Name: deployment.Name,
			}
		}
	}

	if statefulSets, err := c.client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, sts := range statefulSets.Items {
			uid := string(sts.UID)
			nsData.StatefulSets[uid] = &ResourceIdentifier{
				Name: sts.Name,
			}
		}
	}

	if daemonSets, err := c.client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, ds := range daemonSets.Items {
			uid := string(ds.UID)
			nsData.DaemonSets[uid] = &ResourceIdentifier{
				Name: ds.Name,
			}
		}
	}

	if replicaSets, err := c.client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, rs := range replicaSets.Items {
			uid := string(rs.UID)
			nsData.ReplicaSets[uid] = &ResourceIdentifier{
				Name: rs.Name,
			}
		}
	}

	// Capture other resources
	c.captureOtherResources(ctx, namespace, nsData)

	return nil
}

func (c *ClusterSnapshotter) captureOtherResources(ctx context.Context, namespace string, nsData *Namespace) {
	if services, err := c.client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, svc := range services.Items {
			uid := string(svc.UID)
			nsData.Services[uid] = &ResourceIdentifier{Name: svc.Name}
		}
	}

	if secrets, err := c.client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, secret := range secrets.Items {
			uid := string(secret.UID)
			nsData.Secrets[uid] = &ResourceIdentifier{Name: secret.Name}
		}
	}

	if pvcs, err := c.client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, pvc := range pvcs.Items {
			uid := string(pvc.UID)
			nsData.Pvcs[uid] = &ResourceIdentifier{Name: pvc.Name}
		}
	}

	if jobs, err := c.client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, job := range jobs.Items {
			uid := string(job.UID)
			nsData.Jobs[uid] = &ResourceIdentifier{Name: job.Name}
		}
	}

	if cronJobs, err := c.client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, cronJob := range cronJobs.Items {
			uid := string(cronJob.UID)
			nsData.CronJobs[uid] = &ResourceIdentifier{Name: cronJob.Name}
		}
	}

	if ingresses, err := c.client.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, ingress := range ingresses.Items {
			uid := string(ingress.UID)
			nsData.Ingresses[uid] = &ResourceIdentifier{Name: ingress.Name}
		}
	}

	if networkPolicies, err := c.client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, np := range networkPolicies.Items {
			uid := string(np.UID)
			nsData.NetworkPolicies[uid] = &ResourceIdentifier{Name: np.Name}
		}
	}

	if serviceAccounts, err := c.client.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, sa := range serviceAccounts.Items {
			uid := string(sa.UID)
			nsData.ServiceAccounts[uid] = &ResourceIdentifier{Name: sa.Name}
		}
	}

	if roles, err := c.client.RbacV1().Roles(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, role := range roles.Items {
			uid := string(role.UID)
			nsData.Roles[uid] = &ResourceIdentifier{Name: role.Name}
		}
	}

	if roleBindings, err := c.client.RbacV1().RoleBindings(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, rb := range roleBindings.Items {
			uid := string(rb.UID)
			nsData.RoleBindings[uid] = &ResourceIdentifier{Name: rb.Name}
		}
	}

	if pdbs, err := c.client.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, pdb := range pdbs.Items {
			uid := string(pdb.UID)
			nsData.PodDisruptionBudgets[uid] = &ResourceIdentifier{Name: pdb.Name}
		}
	}

	if endpoints, err := c.client.CoreV1().Endpoints(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, ep := range endpoints.Items {
			uid := string(ep.UID)
			nsData.Endpoints[uid] = &ResourceIdentifier{Name: ep.Name}
		}
	}

	if limitRanges, err := c.client.CoreV1().LimitRanges(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, lr := range limitRanges.Items {
			uid := string(lr.UID)
			nsData.LimitRanges[uid] = &ResourceIdentifier{Name: lr.Name}
		}
	}

	if resourceQuotas, err := c.client.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, rq := range resourceQuotas.Items {
			uid := string(rq.UID)
			nsData.ResourceQuotas[uid] = &ResourceIdentifier{Name: rq.Name}
		}
	}

	// Capture HorizontalPodAutoscalers
	if hpas, err := c.client.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, hpa := range hpas.Items {
			uid := string(hpa.UID)
			nsData.HorizontalPodAutoscalers[uid] = &ResourceIdentifier{Name: hpa.Name}
		}
	}

	if csiStorageCapacities, err := c.client.StorageV1().CSIStorageCapacities(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, csc := range csiStorageCapacities.Items {
			uid := string(csc.UID)
			nsData.CsiStorageCapacities[uid] = &ResourceIdentifier{Name: csc.Name}
		}
	}
}

func (c *ClusterSnapshotter) getTargetNamespaces(ctx context.Context) []string {
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		return c.namespaces
	}

	namespaces, err := c.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error(err, "Failed to list namespaces")
		return []string{}
	}

	var nsNames []string
	for _, ns := range namespaces.Items {
		nsNames = append(nsNames, ns.Name)
	}
	return nsNames
}

func (c *ClusterSnapshotter) calculateNamespaceHash(nsData *Namespace) string {
	h := sha256.New()
	if nsBytes, err := json.Marshal(nsData); err == nil {
		h.Write(nsBytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
