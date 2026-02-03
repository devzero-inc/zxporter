package snap

import (
	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// aliases for proto types to maintain compatibility for now
type (
	ResourceIdentifier    = gen.ResourceIdentifier
	ClusterSnapshot       = gen.ClusterSnapshot
	ClusterInfo           = gen.ClusterInfo
	NodeData              = gen.NodeData
	Namespace             = gen.Namespace
	ClusterScopedSnapshot = gen.ClusterScopedSnapshot
)
