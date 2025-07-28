package snap

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *ClusterSnapshotter) captureNodes(ctx context.Context, snapshot *ClusterSnapshot) error {
	nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	allPods, err := c.getAllPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to get all pods: %w", err)
	}

	for _, node := range nodes.Items {
		if c.excludedNodes[node.Name] {
			continue
		}

		nodeData := &NodeData{
			Node: &ResourceIdentifier{
				Name: node.Name,
			},
			Pods: make(map[string]*ResourceIdentifier),
		}

		// Find pods assigned to this node, using UID as key
		for _, pod := range allPods {
			if pod.Spec.NodeName == node.Name && !c.isPodExcluded(pod) {
				uid := string(pod.UID)
				nodeData.Pods[uid] = &ResourceIdentifier{
					Name: pod.Name,
				}
			}
		}

		nodeData.Hash = c.calculateNodeHash(nodeData)
		snapshot.Nodes[string(node.UID)] = nodeData
	}

	snapshot.ClusterInfo.NodeCount = int32(len(snapshot.Nodes))
	return nil
}

func (c *ClusterSnapshotter) calculateNodeHash(nodeData *NodeData) string {
	h := sha256.New()
	if nodeBytes, err := json.Marshal(nodeData); err == nil {
		h.Write(nodeBytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
