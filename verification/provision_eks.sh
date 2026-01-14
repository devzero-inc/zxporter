#!/usr/bin/env bash
set -euo pipefail

need() { command -v "$1" >/dev/null 2>&1 || { echo "Missing dependency: $1" >&2; exit 1; }; }
need aws

# Prefer Homebrew eksctl if available (to bypass outdated /usr/local/bin versions)
if [ -x /opt/homebrew/bin/eksctl ]; then
  EKSCTL_CMD="/opt/homebrew/bin/eksctl"
else
  need eksctl
  EKSCTL_CMD="eksctl"
fi
echo "Using eksctl: $($EKSCTL_CMD version) ($EKSCTL_CMD)"

if [[ "${CNI_TYPE:-aws-cni}" == "cilium" ]]; then
  need helm
fi

MODE="${1:-}"
if [[ "${MODE}" != "create" && "${MODE}" != "delete" ]]; then
  echo "Usage: $0 create|delete" >&2
  exit 1
fi

# ---- enforce self-hosted profile only ----
export AWS_PROFILE="${AWS_PROFILE:-self-hosted}"

# Validate auth works
aws sts get-caller-identity >/dev/null || {
  echo "ERROR: AWS auth failed for profile '${AWS_PROFILE}'. Run:" >&2
  echo "  aws sso login --profile self-hosted" >&2
  exit 1
}

# ---- Config Defaults ----
REGION="${REGION:-ap-south-1}"
K8S_VERSION="${K8S_VERSION:-1.32}"
# Use t4g.small (ARM) by default as requested. Ensure your build matches this architecture!
INSTANCE_TYPE="${INSTANCE_TYPE:-t4g.small}"
CAPACITY_TYPE="${CAPACITY_TYPE:-SPOT}" 
AZ1="${AZ1:-${REGION}a}"
AZ2="${AZ2:-${REGION}b}"
# CNI_TYPE: 'aws-cni' (default) or 'cilium' (AWS VPC CNI + Cilium Chaining)
CNI_TYPE="${CNI_TYPE:-aws-cni}"

WHO="$(whoami | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' '-' | sed 's/^-//; s/-$//')"
CLUSTER_NAME_DEFAULT="zxporter-e2e-${WHO}-${CNI_TYPE}"
CLUSTER_NAME="${CLUSTER_NAME:-${CLUSTER_NAME_DEFAULT}}"

STATE_DIR="${STATE_DIR:-$HOME/.cache/zxporter-e2e}"
STATE_FILE="${STATE_FILE:-${STATE_DIR}/eks.state}"
mkdir -p "${STATE_DIR}"

write_state() {
  cat > "${STATE_FILE}" <<EOF
CLUSTER_NAME=${CLUSTER_NAME}
REGION=${REGION}
CNI_TYPE=${CNI_TYPE}
EOF
}

read_state() {
  [[ -f "${STATE_FILE}" ]] && source "${STATE_FILE}"
}

cluster_exists() {
  $EKSCTL_CMD get cluster --region "${REGION}" --name "${CLUSTER_NAME}" >/dev/null 2>&1
}

# ---------- k8s pre-delete cleanup ----------
k8s_pre_delete_cleanup() {
  command -v kubectl >/dev/null 2>&1 || return 0

  echo "==> In-cluster PVC/PV/namespace cleanup (best-effort)"
  set +e
  aws eks update-kubeconfig --region "${REGION}" --name "${CLUSTER_NAME}" >/dev/null 2>&1
  
  # Delete workloads first to release PVCs
  kubectl delete deployment,daemonset,statefulset,job,pod -A --all >/dev/null 2>&1 || true
  
  kubectl delete pvc -A --all >/dev/null 2>&1 || true
  kubectl delete pv --all >/dev/null 2>&1 || true
  kubectl delete svc -A --all >/dev/null 2>&1 || true
  
  # Delete non-system namespaces
  for ns in $(kubectl get ns -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
    | grep -vE '^(default|kube-system|kube-public|kube-node-lease)$'); do
    kubectl delete ns "${ns}" --wait=false >/dev/null 2>&1 || true
  done
  set -e
}

# ---------- ultra-paranoid AWS cleanup ----------
aws_cleanup_orphans() {
  echo "==> AWS orphan sweep (cluster-tagged resources)"

  set +e

  # EBS volumes
  for v in $(aws ec2 describe-volumes --region "${REGION}" \
      --filters "Name=tag:kubernetes.io/cluster/${CLUSTER_NAME},Values=owned,shared" \
      --query "Volumes[].VolumeId" --output text 2>/dev/null); do
    aws ec2 delete-volume --region "${REGION}" --volume-id "${v}" || true
  done

  # Snapshots
  for s in $(aws ec2 describe-snapshots --region "${REGION}" --owner-ids self \
      --filters "Name=tag:kubernetes.io/cluster/${CLUSTER_NAME},Values=owned,shared" \
      --query "Snapshots[].SnapshotId" --output text 2>/dev/null); do
    aws ec2 delete-snapshot --region "${REGION}" --snapshot-id "${s}" || true
  done

  # Security Groups (often stuck)
  # Basic logic: find SGs tagged with cluster, try delete. 
  # Might fail if dependencies exist, handled by eksctl delete usually, this is just backup.
  for sg in $(aws ec2 describe-security-groups --region "${REGION}" \
      --filters "Name=tag:kubernetes.io/cluster/${CLUSTER_NAME},Values=owned,shared" \
      --query "SecurityGroups[].GroupId" --output text 2>/dev/null); do
    aws ec2 delete-security-group --region "${REGION}" --group-id "${sg}" || true
  done

  set -e
}

# ---------- create ----------
if [[ "${MODE}" == "create" ]]; then
  if cluster_exists; then
      echo "Cluster ${CLUSTER_NAME} already exists."
      write_state
      exit 0
  fi

  # Generate eksctl config
  # Note: using AmazonLinux2 for wider compatibility if AL2023 has issues, but user asked for cheap/modern.
  # capacityType: SPOT is correct for managedNodeGroups in recent eksctl.
  # If it fails, check eksctl version.
  
  cat > /tmp/eksctl-zxporter-e2e.yaml <<EOF
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig
metadata:
  name: ${CLUSTER_NAME}
  region: ${REGION}
  version: "${K8S_VERSION}"

# TODO (debo): still requires testing since this is becoming the default for eksctl in upcoming releases
# autoModeConfig:
#   enabled: false

availabilityZones: ["${AZ1}", "${AZ2}"]

iam:
  withOIDC: true

vpc:
  nat:
    gateway: Disable # Cheap: No NAT Gateway. Nodes must have public IPs to reach logs/internet if needed, or use endpoints?
    # Actually, managed nodes in public subnets need auto-assign public IP which eksctl does by default if no private networking.

managedNodeGroups:
  - name: ng-spot-1
    availabilityZones: ["${AZ1}"]
    instanceType: ${INSTANCE_TYPE}
    minSize: 1
    maxSize: 1
    desiredCapacity: 1
    volumeSize: 20
    # Spot configuration
    spot: true 
    # capacityType: ${CAPACITY_TYPE} # Commented out to fallback to 'spot: true' if capacityType fails parsing in older eksctl
    
  - name: ng-spot-2
    availabilityZones: ["${AZ2}"]
    instanceType: ${INSTANCE_TYPE}
    minSize: 1
    maxSize: 1
    desiredCapacity: 1
    volumeSize: 20
    spot: true
EOF

  echo "Creating cluster ${CLUSTER_NAME} (${REGION}) with ${INSTANCE_TYPE} (Spot)..."
  $EKSCTL_CMD create cluster -f /tmp/eksctl-zxporter-e2e.yaml

  # Post-Provisioning Steps
  if [[ "${CNI_TYPE}" == "cilium" ]]; then
    echo "Installing Cilium CNI (Chaining Mode)..."
    need helm
    
    # Update kubeconfig first
    aws eks update-kubeconfig --region "${REGION}" --name "${CLUSTER_NAME}"
    
    helm repo add cilium https://helm.cilium.io/ >/dev/null 2>&1 || true
    helm repo update >/dev/null 2>&1
    
    # Install Cilium in chaining mode (co-exist with AWS VPC CNI)
    # This enables eBPF datapath/monitoring while keeping AWS IPAM
    helm upgrade --install cilium cilium/cilium \
      --version 1.14.7 \
      --namespace kube-system \
      --set cni.chainingMode=aws-cni \
      --set cni.exclusive=false \
      --set enableIPv4Masquerade=false \
      --set routingMode=native \
      --set endpointRoutes.enabled=true \
      --wait
      
    echo "Cilium installed. Verifying pods..."
    kubectl -n kube-system wait --for=condition=ready pod -l k8s-app=cilium --timeout=300s
    kubectl -n kube-system get pods -l k8s-app=cilium -o wide
    echo "Cilium is ready."
  fi

  write_state
  exit 0
fi

# ---------- delete ----------
read_state
CLUSTER_NAME="${CLUSTER_NAME:-${CLUSTER_NAME_DEFAULT}}"
REGION="${REGION:-ap-south-1}"

echo "Deleting cluster ${CLUSTER_NAME} (${REGION})"

k8s_pre_delete_cleanup
$EKSCTL_CMD delete cluster --region "${REGION}" --name "${CLUSTER_NAME}" --wait || true
aws_cleanup_orphans
rm -f "${STATE_FILE}" || true

echo "Delete complete."
