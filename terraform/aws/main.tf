provider "aws" {
  region = "us-east-1"
}

data "aws_caller_identity" "current" {}

# VPC Configuration
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

  public_subnet_tags = {
    "kubernetes.io/cluster/${var.cluster_name}" = "shared"
    "kubernetes.io/role/elb"                    = "1"
  }

  private_subnet_tags = {
    "kubernetes.io/cluster/${var.cluster_name}" = "shared"
    "kubernetes.io/role/internal-elb"           = "1"
    "karpenter.sh/discovery"                    = "${var.cluster_name}" 
  }
}

# IAM Roles and Policies for Karpenter
resource "aws_iam_role" "karpenter_node_role" {
  name = "KarpenterNodeRole-${var.cluster_name}"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Principal = {
          Service = "ec2.amazonaws.com"
        }
        Action   = "sts:AssumeRole"
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "karpenter_node_role_policy_attachment" {
  role       = aws_iam_role.karpenter_node_role.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
}

resource "aws_iam_role_policy_attachment" "karpenter_node_ssm_policy_attachment" {
  role       = aws_iam_role.karpenter_node_role.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy_attachment" "karpenter_node_registry_policy_attachment" {
  role       = aws_iam_role.karpenter_node_role.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly"
}

resource "aws_iam_role_policy_attachment" "karpenter_node_admin_policy_attachment" {
  role       = aws_iam_role.karpenter_node_role.name
  policy_arn = "arn:aws:iam::aws:policy/AdministratorAccess"
}

resource "aws_iam_role" "karpenter_controller_role" {
  name = "KarpenterControllerRole-${var.cluster_name}"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Principal = {
          Federated = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:oidc-provider/oidc.eks.${var.region}.amazonaws.com/id/${split("/id/", module.eks.cluster_oidc_issuer_url)[1]}"
        }
        Action   = "sts:AssumeRoleWithWebIdentity"
        Condition = {
          StringEquals = {
            "oidc.eks.${var.region}.amazonaws.com/id/${split("/id/", module.eks.cluster_oidc_issuer_url)[1]}:sub" = "system:serviceaccount:kube-system:karpenter"
          }
        }
      }
    ]
  })
}

resource "aws_iam_policy" "karpenter_controller_policy" {
  name        = "KarpenterControllerPolicy-${var.cluster_name}"
  description = "Custom Karpenter controller policy for managing EC2 instances, IAM roles, and EKS."

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Action = [
          "ssm:GetParameter",
          "ec2:DescribeImages",
          "ec2:RunInstances",
          "ec2:DescribeSubnets",
          "ec2:DescribeSecurityGroups",
          "ec2:DescribeLaunchTemplates",
          "ec2:DescribeInstances",
          "ec2:DescribeInstanceTypes",
          "ec2:DescribeInstanceTypeOfferings",
          "ec2:DeleteLaunchTemplate",
          "ec2:CreateTags",
          "ec2:CreateLaunchTemplate",
          "ec2:CreateFleet",
          "ec2:DescribeSpotPriceHistory",
          "pricing:GetProducts"
        ]
        Effect = "Allow"
        Resource = "*"
        Sid = "Karpenter"
      },
      {
        Action = "ec2:TerminateInstances"
        Condition = {
          StringLike = {
            "ec2:ResourceTag/karpenter.sh/nodepool" = "*"
          }
        }
        Effect = "Allow"
        Resource = "*"
        Sid = "ConditionalEC2Termination"
      },
      {
        Effect = "Allow"
        Action = "iam:PassRole"
        Resource = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/KarpenterNodeRole-${var.cluster_name}"
        Sid = "PassNodeIAMRole"
      },
      {
        Effect = "Allow"
        Action = "eks:DescribeCluster"
        Resource = "arn:aws:eks:${var.region}:${data.aws_caller_identity.current.account_id}:cluster/${var.cluster_name}"
        Sid = "EKSClusterEndpointLookup"
      },
      {
        Sid = "AllowScopedInstanceProfileCreationActions"
        Effect = "Allow"
        Resource = "*"
        Action = ["iam:CreateInstanceProfile"]
        Condition = {
          StringEquals = {
            "aws:RequestTag/kubernetes.io/cluster/${var.cluster_name}" = "owned"
            "aws:RequestTag/topology.kubernetes.io/region"           = "${var.region}"
          }
          StringLike = {
            "aws:RequestTag/karpenter.k8s.aws/ec2nodeclass" = "*"
          }
        }
      },
      {
        Sid = "AllowScopedInstanceProfileTagActions"
        Effect = "Allow"
        Resource = "*"
        Action = ["iam:TagInstanceProfile"]
        Condition = {
          StringEquals = {
            "aws:ResourceTag/kubernetes.io/cluster/${var.cluster_name}" = "owned"
            "aws:ResourceTag/topology.kubernetes.io/region"           = "${var.region}"
            "aws:RequestTag/kubernetes.io/cluster/${var.cluster_name}" = "owned"
            "aws:RequestTag/topology.kubernetes.io/region"           = "${var.region}"
          }
          StringLike = {
            "aws:ResourceTag/karpenter.k8s.aws/ec2nodeclass" = "*"
            "aws:RequestTag/karpenter.k8s.aws/ec2nodeclass" = "*"
          }
        }
      },
      {
        Sid = "AllowScopedInstanceProfileActions"
        Effect = "Allow"
        Resource = "*"
        Action = [
          "iam:AddRoleToInstanceProfile",
          "iam:RemoveRoleFromInstanceProfile",
          "iam:DeleteInstanceProfile"
        ]
        Condition = {
          StringEquals = {
            "aws:ResourceTag/kubernetes.io/cluster/${var.cluster_name}" = "owned"
            "aws:ResourceTag/topology.kubernetes.io/region"           = "${var.region}"
          }
          StringLike = {
            "aws:ResourceTag/karpenter.k8s.aws/ec2nodeclass" = "*"
          }
        }
      },
      {
        Sid = "AllowInstanceProfileReadActions"
        Effect = "Allow"
        Resource = "*"
        Action = "iam:GetInstanceProfile"
      },
      {
        Effect = "Allow"
        Action = [
          "sqs:DeleteMessage",
          "sqs:GetQueueUrl",
          "sqs:GetQueueAttributes",
          "sqs:ReceiveMessage"
        ]
        Resource = "*"
        Sid      = "KarpenterInterruptionQueue"
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "karpenter_controller_custom_policy_attachment" {
  role       = aws_iam_role.karpenter_controller_role.name
  policy_arn = aws_iam_policy.karpenter_controller_policy.arn
}


resource "aws_iam_role_policy_attachment" "karpenter_controller_policy_attachment" {
  role       = aws_iam_role.karpenter_controller_role.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

resource "aws_iam_role_policy_attachment" "karpenter_controller_admin_policy_attachment" {
  role       = aws_iam_role.karpenter_controller_role.name
  policy_arn = "arn:aws:iam::aws:policy/AdministratorAccess"
}

# EKS Cluster Configuration
module "eks" {
  source          = "terraform-aws-modules/eks/aws"

  cluster_name    = var.cluster_name
  cluster_version = var.cluster_version

  # Add VPC configuration
  vpc_id          = module.vpc.vpc_id
  subnet_ids      = module.vpc.private_subnets

  enable_irsa     = true
  enable_cluster_creator_admin_permissions = true
  cluster_endpoint_public_access = true
  cluster_endpoint_public_access_cidrs = ["0.0.0.0/0"]

  create_node_iam_role = false

  tags = {
    "karpenter.sh/discovery" = var.cluster_name
  }

  eks_managed_node_groups = {
    gpu_nodes = {
      instance_types = ["g6.4xlarge"]
      desired_size   = 1
      min_size       = 1
      max_size       = 1

      ami_type       = "AL2023_x86_64_NVIDIA"
      use_custom_launch_template = false

      metadata_options = {
        http_endpoint               = "enabled"
        http_tokens                 = "optional" 
        http_put_response_hop_limit = 2           
        instance_metadata_tags      = "enabled"
      }

      disk_size      = 200
      labels = {
        node_type = "gpu"
      }

      # Attach the IAM role for Karpenter to the managed node group
      iam_instance_profile = aws_iam_role.karpenter_node_role.name
    }
  }
}

resource "aws_security_group" "karpenter_sg" {
  name        = "karpenter-sg-${var.cluster_name}"
  description = "Karpenter security group"
  vpc_id      = module.vpc.vpc_id

  tags = {
    "karpenter.sh/discovery" = "${var.cluster_name}"
  }
}

resource "aws_security_group_rule" "karpenter_inbound" {
  security_group_id = aws_security_group.karpenter_sg.id
  type              = "ingress"
  from_port         = 0
  to_port           = 65535
  protocol          = "tcp"
  cidr_blocks       = ["0.0.0.0/0"]
}

resource "aws_sqs_queue" "karpenter_interruption_queue" {
  name = "${var.cluster_name}-karpenter-interruption"
  sqs_managed_sse_enabled = true

  tags = {
    "karpenter.sh/discovery" = var.cluster_name
  }
}

resource "aws_sqs_queue_policy" "karpenter_interruption_queue_policy" {
  queue_url = aws_sqs_queue.karpenter_interruption_queue.url

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "AllowKarpenterController"
        Effect = "Allow"
        Principal = {
          AWS = aws_iam_role.karpenter_controller_role.arn
        }
        Action = [
          "sqs:DeleteMessage",
          "sqs:GetQueueUrl",
          "sqs:GetQueueAttributes",
          "sqs:ReceiveMessage"
        ]
        Resource = aws_sqs_queue.karpenter_interruption_queue.arn
      },
      {
        Sid    = "EC2SpotInterruption"
        Effect = "Allow"
        Principal = {
          Service = ["events.amazonaws.com", "sqs.amazonaws.com"]
        }
        Action   = ["sqs:SendMessage"]
        Resource = aws_sqs_queue.karpenter_interruption_queue.arn
      }
    ]
  })
}

resource "aws_cloudwatch_event_rule" "spot_interruption" {
  name        = "${var.cluster_name}-spot-interruption"
  description = "Capture EC2 Spot Instance interruption notices"

  event_pattern = jsonencode({
    source      = ["aws.ec2"]
    detail-type = ["EC2 Spot Instance Interruption Warning"]
  })
}

resource "aws_cloudwatch_event_target" "spot_interruption" {
  target_id = "KarpenterInterruptionQueueTarget"
  rule      = aws_cloudwatch_event_rule.spot_interruption.name
  arn       = aws_sqs_queue.karpenter_interruption_queue.arn
}