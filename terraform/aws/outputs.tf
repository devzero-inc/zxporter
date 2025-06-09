data "aws_eks_cluster" "eks_cluster" {
  name = var.cluster_name
}

output "oidc_provider_url" {
  value = module.eks.cluster_oidc_issuer_url
}

# Output the cluster endpoint
output "cluster_endpoint" {
  value = data.aws_eks_cluster.eks_cluster.endpoint
  description = "The endpoint of the EKS cluster"
}