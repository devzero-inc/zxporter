#!/bin/bash
set -euo pipefail

NAMESPACE="devzero-zxporter"
RELEASE_NAME="zxporter"
IMAGE_REPO="docker.io/parthiba007/zxporter"
IMAGE_TAG="prom"
CLUSTER_TOKEN="dakr-vQWeSAOpsTVt4pDmHoZeA2SnfFmwlTZhc09csJPM3Kw="
KUBE_CONTEXT="$(kubectl config current-context)"
K8S_PROVIDER="other"

echo "=== Adopting cluster-scoped resources for Helm ==="

for cr in devzero-zxporter-collectionpolicy-editor-role \
          devzero-zxporter-collectionpolicy-viewer-role \
          devzero-zxporter-manager-role \
          devzero-zxporter-metrics-auth-role \
          devzero-zxporter-metrics-reader; do
  echo "  ClusterRole: $cr"
  kubectl label clusterrole "$cr" app.kubernetes.io/managed-by=Helm --overwrite 2>/dev/null || true
  kubectl annotate clusterrole "$cr" \
    meta.helm.sh/release-name="$RELEASE_NAME" \
    meta.helm.sh/release-namespace="$NAMESPACE" --overwrite 2>/dev/null || true
done

for crb in devzero-zxporter-manager-rolebinding \
           devzero-zxporter-metrics-auth-rolebinding; do
  echo "  ClusterRoleBinding: $crb"
  kubectl label clusterrolebinding "$crb" app.kubernetes.io/managed-by=Helm --overwrite 2>/dev/null || true
  kubectl annotate clusterrolebinding "$crb" \
    meta.helm.sh/release-name="$RELEASE_NAME" \
    meta.helm.sh/release-namespace="$NAMESPACE" --overwrite 2>/dev/null || true
done

echo ""
echo "=== Installing zxporter (nodemon mode, no Prometheus) ==="

helm install "$RELEASE_NAME" ./helm-chart/zxporter \
  --namespace "$NAMESPACE" --create-namespace \
  --set image.repository="$IMAGE_REPO" \
  --set image.tag="$IMAGE_TAG" \
  --set zxporter.clusterToken="$CLUSTER_TOKEN" \
  --set zxporter.kubeContextName="$KUBE_CONTEXT" \
  --set zxporter.k8sProvider="$K8S_PROVIDER"

echo ""
echo "=== Installing nodemon DaemonSet ==="

helm install zxporter-nodemon ./helm-chart/zxporter-nodemon \
  --namespace "$NAMESPACE" \
  --set gpuMetricsExporter.image.repository=docker.io/parthiba007/nodemon \
  --set gpuMetricsExporter.image.tag=prom \
  --set provider="$K8S_PROVIDER"

echo ""
echo "=== Done. Checking pods ==="
kubectl get pods -n "$NAMESPACE"
