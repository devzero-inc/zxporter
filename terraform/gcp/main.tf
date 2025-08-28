provider "google" {
  project = var.project_id
  region  = var.region
}

resource "google_compute_network" "gke_vpc" {
  name                    = "${var.cluster_name}-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "gke_subnet" {
  name          = "${var.cluster_name}-subnet"
  ip_cidr_range = "10.10.0.0/16"
  region        = var.region
  network       = google_compute_network.gke_vpc.id

  secondary_ip_range {
    range_name    = "pods-range"
    ip_cidr_range = "10.20.0.0/16"
  }

  secondary_ip_range {
    range_name    = "services-range"
    ip_cidr_range = "10.30.0.0/20"
  }
}

module "gke" {
  source  = "terraform-google-modules/kubernetes-engine/google"

  project_id         = var.project_id
  name               = var.cluster_name
  region             = var.region
  kubernetes_version = var.cluster_version

  network    = google_compute_network.gke_vpc.name
  subnetwork = google_compute_subnetwork.gke_subnet.name

  remove_default_node_pool = true
  create_service_account   = false

  ip_range_pods     = "pods-range"
  ip_range_services = "services-range"

  node_pools = [
    {
      name               = "gpu-nodes"
      machine_type       = "n1-standard-16"
      node_locations     = "us-central1-a"
      min_count          = 1
      max_count          = 1
      initial_node_count = 1
      local_ssd_count    = 0
      disk_size_gb       = 200
      disk_type          = "pd-standard"
      image_type         = "UBUNTU_CONTAINERD"
      auto_repair        = true
      auto_upgrade       = false
      service_account    = "default"
      preemptible        = false
      spot               = false
      accelerator_count  = 1
      accelerator_type   = "nvidia-tesla-t4"
      gpu_driver_version = "DEFAULT"
    }
  ]

  node_pools_metadata = {
    gpu-nodes = {
      node_type = "gpu"
    }
  }

  enable_vertical_pod_autoscaling = false
  enable_binary_authorization     = false
  release_channel                 = "UNSPECIFIED"
  deletion_protection             = false
}
