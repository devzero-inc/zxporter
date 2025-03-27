package provider

import (
	"context"
)

// Provider defines functionality for cloud provider-specific operations
type Provider interface {
	// Name returns the provider's name (eks, gke, aks, etc.)
	Name() string

	// GetClusterMetadata retrieves provider-specific cluster metadata
	GetClusterMetadata(ctx context.Context) (map[string]interface{}, error)

	// IsNodeInNodeGroup determines if a node belongs to a specific node group and returns node group metadata
	GetNodeGroupForNode(nodeName string) (map[string]interface{}, error)
}
