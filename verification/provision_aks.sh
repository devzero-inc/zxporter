#!/usr/bin/env bash
set -euo pipefail

need() { command -v "$1" >/dev/null 2>&1 || { echo "Missing dependency: $1" >&2; exit 1; }; }
need az

MODE="${1:-}"
if [[ "${MODE}" != "create" && "${MODE}" != "delete" ]]; then
  echo "Usage: $0 create|delete" >&2
  exit 1
fi

# ---- Config Defaults ----
REGION="${REGION:-eastus}"
K8S_VERSION="${K8S_VERSION:-1.30}"
NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D2ps_v6}" # ARM64 (Azure Cobalt 100)

# CNI_TYPE: 'kubenet' (default) or 'cilium' (Azure CNI + Cilium Dataplane)
CNI_TYPE="${CNI_TYPE:-cilium}"

WHO="$(whoami | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' '-' | sed 's/^-//; s/-$//')"
RESOURCE_GROUP="aks-zxporter-e2e-rg-${WHO}-${CNI_TYPE}"
CLUSTER_NAME="aks-zxporter-e2e-${WHO}-${CNI_TYPE}"

STATE_DIR="${STATE_DIR:-$HOME/.cache/zxporter-e2e}"
STATE_FILE="${STATE_FILE:-${STATE_DIR}/aks.state}"
mkdir -p "${STATE_DIR}"

write_state() {
  cat > "${STATE_FILE}" <<EOF
RESOURCE_GROUP=${RESOURCE_GROUP}
CLUSTER_NAME=${CLUSTER_NAME}
REGION=${REGION}
CNI_TYPE=${CNI_TYPE}
EOF
}

read_state() {
  [[ -f "${STATE_FILE}" ]] && source "${STATE_FILE}"
}

cluster_exists() {
  az aks show --resource-group "${RESOURCE_GROUP}" --name "${CLUSTER_NAME}" >/dev/null 2>&1
}

# ---------- create ----------
if [[ "${MODE}" == "create" ]]; then
  # 1. Ensure Resource Group exists
  if ! az group show --name "${RESOURCE_GROUP}" >/dev/null 2>&1; then
    echo "Creating Resource Group: ${RESOURCE_GROUP} (${REGION})..."
    az group create --name "${RESOURCE_GROUP}" --location "${REGION}"
  fi

  if cluster_exists; then
      echo "Cluster ${CLUSTER_NAME} already exists."
      write_state
      exit 0
  fi

  echo "Creating AKS cluster ${CLUSTER_NAME} (${REGION})..."
  echo "  VM Size: ${NODE_VM_SIZE}"
  echo "  Nodes: 1 System (Regular) + 1 User (Spot)"
  echo "  CNI: ${CNI_TYPE}"

  # Base Create Command (System Node Pool)
  CMD_ARGS=(
    "--resource-group" "${RESOURCE_GROUP}"
    "--name" "${CLUSTER_NAME}"
    "--node-count" "1"
    "--node-vm-size" "${NODE_VM_SIZE}"
    "--tier" "free"
    "--zones" "2"
    "--generate-ssh-keys"
  )

  # Network Configuration
  if [[ "${CNI_TYPE}" == "cilium" ]]; then
    CMD_ARGS+=(
      "--network-plugin" "azure"
      "--network-dataplane" "cilium"
    )
    echo "  -> Enabled Azure CNI with Cilium Dataplane"
  else
    CMD_ARGS+=(
      "--network-plugin" "kubenet"
    )
    echo "  -> Enabled Kubenet (Basic)"
  fi

  echo "Step 1: Creating Cluster with System Pool..."
  az aks create "${CMD_ARGS[@]}"

  echo "Step 2: Adding Spot Node Pool..."
  az aks nodepool add \
    --resource-group "${RESOURCE_GROUP}" \
    --cluster-name "${CLUSTER_NAME}" \
    --name spotpool \
    --node-count 1 \
    --node-vm-size "${NODE_VM_SIZE}" \
    --priority Spot \
    --eviction-policy Delete \
    --spot-max-price -1 \
    --zones 3 \
    --mode User

  echo "Cluster creation complete."
  write_state
  exit 0
fi

# ---------- delete ----------
read_state
RESOURCE_GROUP="${RESOURCE_GROUP:-zxporter-e2e-rg-${WHO}}"
CLUSTER_NAME="${CLUSTER_NAME:-zxporter-e2e-${WHO}}"

echo "Deleting cluster ${CLUSTER_NAME} in RG ${RESOURCE_GROUP}..."

# Best effort pre-cleanup
if command -v kubectl >/dev/null 2>&1; then
    echo "Fetching credentials for cleanup..."
    az aks get-credentials --resource-group "${RESOURCE_GROUP}" --name "${CLUSTER_NAME}" --overwrite-existing >/dev/null 2>&1 || true
    
    echo "Cleaning up k8s resources (best effort)..."
    kubectl delete svc -A --all --timeout=10s >/dev/null 2>&1 || true
    kubectl delete pvc -A --all --timeout=10s >/dev/null 2>&1 || true
fi

# Delete the entire Resource Group
echo "Deleting Resource Group ${RESOURCE_GROUP} (Background)..."
az group delete --name "${RESOURCE_GROUP}" --yes --no-wait

rm -f "${STATE_FILE}" || true
echo "Deletion initiated in background."
