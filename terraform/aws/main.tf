provider "aws" {
  region = "us-east-1"
}

module "vpc" {
  source = "terraform-aws-modules/vpc/aws"

  name = "${var.cluster_name}-vpc"
  cidr = "10.0.0.0/16"

  azs             = ["us-east-1a", "us-east-1b"]
  private_subnets = ["10.0.1.0/24", "10.0.2.0/24"]
  public_subnets  = ["10.0.101.0/24", "10.0.102.0/24"]

  enable_nat_gateway = true
  single_nat_gateway = true
  
  # Required for EKS
  enable_dns_hostnames = true
  enable_dns_support   = true
}

module "eks" {
  source          = "terraform-aws-modules/eks/aws"

  cluster_name    = var.cluster_name
  cluster_version = var.cluster_version

  # Add VPC configuration
  vpc_id          = module.vpc.vpc_id
  subnet_ids      = module.vpc.private_subnets

  enable_irsa     = true

  cluster_endpoint_public_access = true
  enable_cluster_creator_admin_permissions = true
  cluster_endpoint_public_access_cidrs = ["0.0.0.0/0"]

  eks_managed_node_groups = {
    gpu_nodes = {
      instance_types = ["g6.4xlarge"]
      desired_size   = 1
      min_size      = 1
      max_size      = 1

      ami_type      = "AL2023_x86_64_NVIDIA"

      use_custom_launch_template = false

      disk_size     = 200

      labels = {
        node_type = "gpu"
      }
    }
  }
}
