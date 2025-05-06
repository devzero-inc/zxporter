package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	tagEKSK8sCluster        = "k8s.io/cluster/"
	tagEKSKubernetesCluster = "kubernetes.io/cluster/"
	owned                   = "owned"

	// AWS capacity type label
	awsCapacityTypeLabel = "eks.amazonaws.com/capacityType"

	// AWS node group label
	awsNodeGroupLabel = "eks.amazonaws.com/nodegroup"
)

// AWSProvider implements provider interface for AWS EKS
type AWSProvider struct {
	sync.Mutex

	logger      logr.Logger
	k8sClient   kubernetes.Interface
	metaClient  *ec2metadata.EC2Metadata
	ec2Client   *ec2.EC2
	eksClient   *eks.EKS
	region      string
	accountID   string
	clusterName string
	nodeGroups  map[NodePoolName]map[AWSNodeGroupType][]string
}

// CapacityTypesOnDemand, CapacityTypesSpot or CapacityTypesCapacityBlock
type AWSNodeGroupType string

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
		nodeGroups: make(map[NodePoolName]map[AWSNodeGroupType][]string),
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
	nodeGroups := p.GetNodeGroupMetadata(ctx)
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

func (p *AWSProvider) GetNodeGroupMetadata(ctx context.Context) map[string]map[string][]string {
	nodeList, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil { // failed to call K8s API
		// return cached info, if available
		return awsTypesToGeneric(p.nodeGroups)
	}

	refreshedNodeGroupMetadata := getAWSNodeGroupMetadata(nodeList)
	p.Lock()
	p.nodeGroups = refreshedNodeGroupMetadata
	p.Unlock()

	return awsTypesToGeneric(p.nodeGroups)
}

func getAWSNodeGroupMetadata(nodeList *corev1.NodeList) map[NodePoolName]map[AWSNodeGroupType][]string {
	nodeGroupInfo := make(map[NodePoolName]map[AWSNodeGroupType][]string)
	if nodeList == nil {
		return nodeGroupInfo
	}

	for _, node := range nodeList.Items {
		nodePoolLabel := getAWSNodePoolLabel(&node)
		nodePoolScaleSet := getAWSNodeGroupType(node)
		// update node count in node pool
		if _, ok := nodeGroupInfo[nodePoolLabel]; !ok {
			nodeGroupInfo[nodePoolLabel] = make(map[AWSNodeGroupType][]string)
		}

		if _, ok := nodeGroupInfo[nodePoolLabel][nodePoolScaleSet]; !ok {
			nodeGroupInfo[nodePoolLabel][nodePoolScaleSet] = []string{}
		}
		nodeGroupInfo[nodePoolLabel][nodePoolScaleSet] = append(nodeGroupInfo[nodePoolLabel][nodePoolScaleSet], node.Name)
	}
	return nodeGroupInfo
}

func getAWSNodePoolLabel(node *corev1.Node) NodePoolName {
	if val, ok := node.Labels[awsCapacityTypeLabel]; ok {
		return NodePoolName(val)
	}
	return NodePoolName("unknown")
}

func getAWSNodeGroupType(node corev1.Node) AWSNodeGroupType {
	if val, ok := node.Labels[awsNodeGroupLabel]; ok {
		return AWSNodeGroupType(val)
	}
	return eks.CapacityTypesOnDemand // "on-demand"
}

func awsTypesToGeneric(metadata map[NodePoolName]map[AWSNodeGroupType][]string) map[string]map[string][]string {
	m := make(map[string]map[string][]string)
	if metadata == nil {
		return m
	}

	for nodePool, nodePoolTypeToNodeList := range metadata {
		m[string(nodePool)] = make(map[string][]string)
		for nodePoolType, nodeList := range nodePoolTypeToNodeList {
			m[string(nodePool)][string(nodePoolType)] = nodeList
		}
	}
	return m
}
