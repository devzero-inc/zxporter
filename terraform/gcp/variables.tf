variable "project_id" {
  type        = string
  description = "GCP project ID"
}

variable "region" {
  type        = string
  default     = "us-central1"
  description = "GCP region"
}

variable "cluster_name" {
  type        = string
  description = "Name of the GKE cluster"
}

variable "cluster_version" {
  type        = string
  description = "Kubernetes version"
}
