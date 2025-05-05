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

	/*
		Azure:
			{
				node_pool_1: {
					Regular: [vm1, vm2]
				},
				node_pool_2: {
					Spot: [vm5, vm6]
				},
				node_pool_3: {
					Regular: [vm3, vm4],
					Spot: [vm7, vm9]
				}
			}

		AWS:
			{
				node_group_1: {
					Regular: [vm1, vm2]
				},
				node_group_2: {
					Spot: [vm5, vm6]
				},
				node_group_3: {
					Regular: [vm3, vm4],
					Spot: [vm7, vm9]
				}
			}

		GKE:
			{
				nodepool_1: {
					Regular: [vm1, vm2]
				},
				nodepool_2: {
					Spot: [vm5, vm6]
				},
				nodepool_3: {
					Regular: [vm3, vm4],
					Spot: [vm7, vm9]
				}
			}
	*/
	// node-{pool,group} name maps to map of node type {spot, regular} to list of nodes/VMs
	GetNodeGroupMetadata(context.Context) map[string]map[string][]string
}
