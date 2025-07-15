package snap

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *ClusterSnapshotter) getAllPods(ctx context.Context) ([]*corev1.Pod, error) {
	var allPods []*corev1.Pod

	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		for _, ns := range c.namespaces {
			pods, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to list pods in namespace %s: %w", ns, err)
			}
			for _, pod := range pods.Items {
				allPods = append(allPods, pod.DeepCopy())
			}
		}
	} else {
		pods, err := c.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list all pods: %w", err)
		}
		for _, pod := range pods.Items {
			allPods = append(allPods, pod.DeepCopy())
		}
	}

	return allPods, nil
}

func (c *ClusterSnapshotter) isPodExcluded(pod *corev1.Pod) bool {
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	return c.excludedPods[key]
}
