#!/usr/bin/env bash
set -euo pipefail

need() { command -v "$1" >/dev/null 2>&1 || { echo "Missing dependency: $1" >&2; exit 1; }; }
need gcloud

MODE="${1:-}"
if [[ "${MODE}" != "create" && "${MODE}" != "delete" ]]; then
  echo "Usage: $0 create|delete" >&2
  exit 1
fi

# ---- Config Defaults ----
echo "Using GCP Project: $(gcloud config get-value project)"
REGION="${REGION:-us-central1}"
# T2A is GKE's ARM64 (Tau) instance. Available in: us-central1, europe-west4, asia-southeast1
NODE_MACHINE_TYPE="${NODE_MACHINE_TYPE:-t2a-standard-1}" 
# Image type must be optimized for ARM64 if using T2A
IMAGE_TYPE="${IMAGE_TYPE:-COS_CONTAINERD}" 

WHO="$(whoami | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' '-' | sed 's/^-//; s/-$//')"
CLUSTER_NAME="gke-zxporter-e2e-${WHO}"

STATE_DIR="${STATE_DIR:-$HOME/.cache/zxporter-e2e}"
STATE_FILE="${STATE_FILE:-${STATE_DIR}/gke.state}"
mkdir -p "${STATE_DIR}"

write_state() {
  cat > "${STATE_FILE}" <<EOF
REGION=${REGION}
ZONE=${ZONE}
CLUSTER_NAME=${CLUSTER_NAME}
EOF
}

load_state() {
  if [[ -f "${STATE_FILE}" ]]; then
    source "${STATE_FILE}"
  fi
}

cluster_exists() {
  gcloud container clusters describe "${CLUSTER_NAME}" --zone "${REGION}-a" >/dev/null 2>&1
}

if [[ "${MODE}" == "create" ]]; then
  if cluster_exists; then
      echo "Cluster ${CLUSTER_NAME} already exists."
      write_state
      exit 0
  fi

  echo "Creating GKE cluster ${CLUSTER_NAME} (${REGION})..."
  echo "  Machine Type: ${NODE_MACHINE_TYPE}"
  echo "  Image Type: ${IMAGE_TYPE}"

  # Creating a zonal cluster or regional? Regional is expensive/slower for E2E.
  # We'll stick to a specific zone for the main pool to ensure T2A availability (e.g. us-central1-a)
  # But the --region cli creates a regional control plane.
  
  # Note: T2A might not be in all zones of us-central1. us-central1-a,b,f usually.
  # Let's target a specific zone to simplify.
  ZONE="${REGION}-a"

  # Create Cluster with Default Pool (System)
  # We'll use --num-nodes 1 per zone. If --zone is used, 1 node.
  gcloud container clusters create "${CLUSTER_NAME}" \
    --zone "${ZONE}" \
    --num-nodes 1 \
    --machine-type "${NODE_MACHINE_TYPE}" \
    --image-type "${IMAGE_TYPE}" \
    --disk-size 20GB \
    --disk-type pd-standard \
    --enable-ip-alias \
    --labels="purpose=zxporter-e2e,owner=${WHO}" \
    --no-enable-autoupgrade \
    --no-enable-autorepair

  # Add Spot Pool (Preemptible/Spot)
  echo "Adding Spot Node Pool..."
  gcloud container node-pools create spotpool \
    --cluster "${CLUSTER_NAME}" \
    --zone "${ZONE}" \
    --num-nodes 1 \
    --machine-type "${NODE_MACHINE_TYPE}" \
    --image-type "${IMAGE_TYPE}" \
    --spot \
    --disk-size 20GB \
    --labels="purpose=zxporter-e2e-spot"

  echo "Cluster creation complete."
  write_state
  
  # Get Credentials
  gcloud container clusters get-credentials "${CLUSTER_NAME}" --zone "${ZONE}"
  exit 0
fi

if [[ "${MODE}" == "delete" ]]; then
  load_state
  if ! cluster_exists; then
      echo "Cluster ${CLUSTER_NAME} does not exist."
      exit 0
  fi

  echo "Deleting GKE cluster ${CLUSTER_NAME}..."
  # Assuming Zonal cluster from creation
  ZONE="${REGION}-a"
  
  gcloud container clusters delete "${CLUSTER_NAME}" --zone "${ZONE}" --quiet --async
  echo "Deletion initiated in background."
  rm -f "${STATE_FILE}"
fi
