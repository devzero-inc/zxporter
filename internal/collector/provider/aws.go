package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
)

const (
	tagEKSK8sCluster        = "k8s.io/cluster/"
	tagEKSKubernetesCluster = "kubernetes.io/cluster/"
	owned                   = "owned"
)

// AWSProvider implements provider interface for AWS EKS
type AWSProvider struct {
	logger      logr.Logger
	k8sClient   kubernetes.Interface
	metaClient  *ec2metadata.EC2Metadata
	ec2Client   *ec2.EC2
	eksClient   *eks.EKS
	region      string
	accountID   string
	clusterName string
	nodeGroups  map[string]map[string]interface{}
}

// NewAWSProvider creates a new AWS provider
func NewAWSProvider(logger logr.Logger, k8sClient kubernetes.Interface) (*AWSProvider, error) {
	// Set up AWS session
	sess, err := session.NewSession()
	if err != nil {
		return nil, fmt.Errorf("creating AWS session: %w", err)
	}

	metaClient := ec2metadata.New(sess)

	// Check if running on AWS
	if !metaClient.Available() {
		return nil, fmt.Errorf("EC2 metadata service not available")
	}

	// Get region for EC2 client
	region, err := metaClient.Region()
	if err != nil {
		return nil, fmt.Errorf("getting region from metadata: %w", err)
	}

	// Create EC2 client
	ec2Client := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	// Create EKS client
	eksClient := eks.New(sess, &aws.Config{Region: aws.String(region)})

	return &AWSProvider{
		logger:     logger,
		k8sClient:  k8sClient,
		metaClient: metaClient,
		ec2Client:  ec2Client,
		eksClient:  eksClient,
		region:     region,
		nodeGroups: make(map[string]map[string]interface{}),
	}, nil
}

// Name returns the provider name
func (p *AWSProvider) Name() string {
	return "eks"
}

// GetClusterMetadata retrieves EKS cluster metadata
func (p *AWSProvider) GetClusterMetadata(ctx context.Context) (map[string]interface{}, error) {
	p.logger.Info("Collecting AWS EKS cluster metadata")

	// Get instance ID
	instanceID, err := p.metaClient.GetMetadataWithContext(ctx, "instance-id")
	if err != nil {
		return nil, fmt.Errorf("getting instance ID: %w", err)
	}

	// Get instance details from EC2 API
	result, err := p.ec2Client.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	if err != nil {
		return nil, fmt.Errorf("describing instance: %w", err)
	}

	if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("no instance found with ID %s", instanceID)
	}

	instance := result.Reservations[0].Instances[0]

	// Find cluster name from tags
	var clusterName string
	for _, tag := range instance.Tags {
		if tag.Key != nil && tag.Value != nil {
			// Check for EKS cluster tags
			for _, prefix := range []string{tagEKSK8sCluster, tagEKSKubernetesCluster} {
				if strings.HasPrefix(*tag.Key, prefix) && *tag.Value == owned {
					clusterName = strings.TrimPrefix(*tag.Key, prefix)
					break
				}
			}
			if clusterName != "" {
				break
			}
		}
	}

	if clusterName == "" {
		return nil, fmt.Errorf("couldn't find EKS cluster name in instance tags")
	}

	p.clusterName = clusterName

	// Get account ID from instance identity document
	identityDoc, err := p.metaClient.GetInstanceIdentityDocumentWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting identity document: %w", err)
	}

	p.accountID = identityDoc.AccountID

	// Get detailed cluster info from EKS API
	clusterInfo, err := p.eksClient.DescribeClusterWithContext(ctx, &eks.DescribeClusterInput{
		Name: aws.String(clusterName),
	})
	if err != nil {
		p.logger.Error(err, "Failed to get cluster details from EKS API, continuing with partial metadata")
	}

	// Get node groups for this cluster
	nodeGroups, err := p.getNodeGroups(ctx)
	if err != nil {
		p.logger.Error(err, "Failed to get node groups, continuing with partial metadata")
	}

	metadata := map[string]interface{}{
		"provider":      "eks",
		"region":        p.region,
		"account_id":    p.accountID,
		"cluster_name":  clusterName,
		"instance_id":   instanceID,
		"instance_type": *instance.InstanceType,
		"vpc_id":        *instance.VpcId,
		"subnet_id":     *instance.SubnetId,
		"node_groups":   nodeGroups,
	}

	// Add cluster info if available
	if clusterInfo != nil && clusterInfo.Cluster != nil {
		metadata["platform_version"] = *clusterInfo.Cluster.PlatformVersion
		metadata["kubernetes_version"] = *clusterInfo.Cluster.Version
		if clusterInfo.Cluster.ResourcesVpcConfig != nil {
			metadata["cluster_vpc_id"] = *clusterInfo.Cluster.ResourcesVpcConfig.VpcId
			metadata["cluster_security_group_id"] = *clusterInfo.Cluster.ResourcesVpcConfig.ClusterSecurityGroupId
			metadata["cluster_endpoint"] = *clusterInfo.Cluster.Endpoint
		}
		if clusterInfo.Cluster.Identity != nil && clusterInfo.Cluster.Identity.Oidc != nil {
			metadata["oidc_issuer"] = *clusterInfo.Cluster.Identity.Oidc.Issuer
		}
	}

	p.logger.Info("Collected EKS cluster metadata",
		"cluster", clusterName,
		"region", p.region,
		"account", p.accountID)

	return metadata, nil
}

// getNodeGroups retrieves all node groups associated with the EKS cluster
func (p *AWSProvider) getNodeGroups(ctx context.Context) ([]map[string]interface{}, error) {
	if p.clusterName == "" {
		return nil, fmt.Errorf("cluster name not set")
	}

	// List node groups in this cluster
	nodeGroupsOutput, err := p.eksClient.ListNodegroupsWithContext(ctx, &eks.ListNodegroupsInput{
		ClusterName: aws.String(p.clusterName),
	})
	if err != nil {
		return nil, fmt.Errorf("listing node groups: %w", err)
	}

	var nodeGroups []map[string]interface{}

	// Get details for each node group
	for _, ngName := range nodeGroupsOutput.Nodegroups {
		ngDetails, err := p.eksClient.DescribeNodegroupWithContext(ctx, &eks.DescribeNodegroupInput{
			ClusterName:   aws.String(p.clusterName),
			NodegroupName: ngName,
		})
		if err != nil {
			p.logger.Error(err, "Failed to get node group details", "nodegroup", *ngName)
			continue
		}

		ng := ngDetails.Nodegroup

		// Get launch template info if available
		var launchTemplate map[string]interface{}
		if ng.LaunchTemplate != nil {
			launchTemplate = map[string]interface{}{
				"id":      *ng.LaunchTemplate.Id,
				"version": *ng.LaunchTemplate.Version,
			}
		}

		// Extract instance types
		var instanceTypes []string
		if ng.InstanceTypes != nil {
			for _, it := range ng.InstanceTypes {
				instanceTypes = append(instanceTypes, *it)
			}
		}

		// Extract scaling config
		var scalingConfig map[string]interface{}
		if ng.ScalingConfig != nil {
			scalingConfig = map[string]interface{}{
				"desired_size": *ng.ScalingConfig.DesiredSize,
				"min_size":     *ng.ScalingConfig.MinSize,
				"max_size":     *ng.ScalingConfig.MaxSize,
			}
		}

		nodeGroup := map[string]interface{}{
			"name":            *ng.NodegroupName,
			"status":          *ng.Status,
			"instance_types":  instanceTypes,
			"scaling_config":  scalingConfig,
			"launch_template": launchTemplate,
			"ami_type":        pointerToString(ng.AmiType),
			"capacity_type":   pointerToString(ng.CapacityType),
			"disk_size":       pointerToInt(ng.DiskSize),
			"created_at":      ngTimeToUnix(ng.CreatedAt),
		}

		nodeGroups = append(nodeGroups, nodeGroup)

		// Store in the nodeGroups map for later use
		p.nodeGroups[*ng.NodegroupName] = nodeGroup
	}

	return nodeGroups, nil
}

// GetNodeGroupForNode determines which node group a node belongs to
func (p *AWSProvider) GetNodeGroupForNode(nodeName string) (map[string]interface{}, error) {
	// First check if we need to load node groups
	if len(p.nodeGroups) == 0 {
		_, err := p.getNodeGroups(context.Background())
		if err != nil {
			return nil, fmt.Errorf("loading node groups: %w", err)
		}
	}

	// For EKS, the node group info is typically in a label on the node
	// We would need to get the node object from K8s API to check this properly
	// This is a simplified version that extracts from node name

	// In EKS, nodes often have the pattern: [nodegroup-name]-[uuid]
	for ngName, ngDetails := range p.nodeGroups {
		if strings.Contains(nodeName, ngName) {
			return ngDetails, nil
		}
	}

	return nil, fmt.Errorf("couldn't find node group for node %s", nodeName)
}

// Helper functions
func pointerToString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func pointerToInt(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

func ngTimeToUnix(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}
