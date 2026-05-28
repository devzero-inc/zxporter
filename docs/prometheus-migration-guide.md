# Prometheus Removal Migration Guide

## What Changed

The new zxporter collects all metrics via the **nodemon DaemonSet** (polling kubelet directly) instead of Prometheus. When you upgrade zxporter, the old Prometheus components become unused and should be cleaned up.

**What gets added:** nodemon DaemonSet (bundled with zxporter)
**What gets removed:** Prometheus server, kube-state-metrics, node-exporter, dz-metrics-server

**What does NOT change:** your namespace, your cluster token, your ConfigMap settings.

---

## Resources to Delete

These are the exact resources deployed by the old zxporter that are no longer needed:

### Prometheus Server

| Kind | Name |
|------|------|
| Deployment | `prometheus-dz-prometheus-server` |
| Service | `prometheus-dz-prometheus-server` |
| ServiceAccount | `prometheus-dz-prometheus-server` |
| ConfigMap | `prometheus-dz-prometheus-server` |
| ClusterRole | `prometheus-dz-prometheus-server` |
| ClusterRoleBinding | `prometheus-dz-prometheus-server` |

### Kube-State-Metrics

| Kind | Name |
|------|------|
| Deployment | `prometheus-kube-state-metrics` |
| Service | `prometheus-kube-state-metrics` |
| ServiceAccount | `prometheus-kube-state-metrics` |
| ClusterRole | `prometheus-kube-state-metrics` |
| ClusterRoleBinding | `prometheus-kube-state-metrics` |

### Node-Exporter

| Kind | Name |
|------|------|
| DaemonSet | `dz-prometheus-node-exporter` |
| Service | `dz-prometheus-node-exporter` |
| ServiceAccount | `dz-prometheus-node-exporter` |

### Metrics-Server (auto-installed by old zxporter entrypoint)

| Kind | Name |
|------|------|
| Deployment | `dz-metrics-server` |
| Service | `dz-metrics-server` |
| ServiceAccount | `dz-metrics-server` |
| ClusterRole | `system:dz-metrics-server-aggregated-reader` |
| ClusterRole | `system:dz-metrics-server` |
| ClusterRoleBinding | `dz-metrics-server:system:auth-delegator` |
| ClusterRoleBinding | `system:dz-metrics-server` |
| RoleBinding | `dz-metrics-server-auth-reader` (in `kube-system`) |

> **Note about `v1beta1.metrics.k8s.io` APIService:** If the old zxporter's `dz-metrics-server` was serving this API, deleting it will break `kubectl top`. Most EKS/GKE/AKS clusters have their own metrics-server in `kube-system` that takes over automatically. Check with: `kubectl get apiservice v1beta1.metrics.k8s.io -o jsonpath='{.spec.service}'`

### Standalone Nodemon (only if previously installed separately via Helm)

| Kind | Name |
|------|------|
| DaemonSet | `zxporter-nodemon` |
| ServiceAccount | `zxporter-nodemon` |
| ConfigMap | `zxporter-nodemon-dcgm-metrics` |
| ConfigMap | `zxporter-nodemon-zxporter-nodemon` |
| ClusterRole | `zxporter-nodemon` |
| ClusterRoleBinding | `zxporter-nodemon` |

---

## How to Upgrade

### Option 1: kubectl (curl from DAKR backend)

This is the standard production upgrade path. The DAKR control plane automatically detects which namespace your zxporter is in and templates the manifest accordingly.

**Step 1: Upgrade zxporter**

```bash
curl -XPOST -H 'Authorization: Bearer <YOUR_PAT>' \
  "https://dakr.devzero.io/dakr/installer-updater" \
  | kubectl apply -f -
```

That's it. The updater:
- Targets the correct namespace automatically (DAKR detects it from the cluster record)
- Does NOT touch the ConfigMap (your settings are preserved)
- Does NOT touch the Secret (your cluster token is preserved)
- Deploys the new zxporter binary + nodemon DaemonSet
- Includes a one-time cleanup Job that deletes the Prometheus resources listed above

**Step 2: Verify**

```bash
# Find your namespace
NS=$(kubectl get deployment -A -l control-plane=controller-manager -o jsonpath='{.items[0].metadata.namespace}')
echo "ZXporter namespace: $NS"

# Check pods
kubectl get pods -n $NS

# Expected: zxporter pods + nodemon pods running. No Prometheus pods.
```

### Option 2: Helm

```bash
# Find your namespace
NS=$(helm list -A | grep zxporter | awk '{print $2}')

# Upgrade in place
helm upgrade zxporter <chart-source> --namespace $NS --reuse-values
```

The Helm chart includes a post-upgrade hook that runs the same cleanup Job.

### Option 3: Fresh install (new cluster)

```bash
curl -XPOST -H 'Authorization: Bearer <YOUR_PAT>' \
  -H "X-Kube-Context-Name: $(kubectl config current-context)" \
  "https://dakr.devzero.io/dakr/installer-manifest?cluster-provider=AWS" \
  | kubectl apply -f -
```

This creates a new cluster record. No cleanup needed since there's nothing old to remove.

---

## Skipping the Cleanup Job

If you don't want the cleanup Job to run (e.g., the cluster never had Prometheus, or you want to clean up manually):

**Helm:**
```bash
helm upgrade zxporter <chart> --namespace $NS --reuse-values --no-hooks
```

**kubectl/manifest (build-time):**
```bash
make build-installer SKIP_PROMETHEUS_CLEANUP=1
```

The cleanup Job is idempotent and uses `--ignore-not-found` — it's safe to run even on clusters that never had Prometheus. All deletes succeed silently.

---

## What the Cleanup Job Does

The Job runs in the **same namespace** as zxporter. It:

1. Creates a dedicated ServiceAccount (`zxporter-prometheus-cleanup`) with least-privilege RBAC
2. Deletes the Prometheus resources **by exact name** (see list above)
3. Self-deletes after 5 minutes (`ttlSecondsAfterFinished: 300`)

**Safety guarantees:**
- Uses `resourceNames` on every RBAC rule — can ONLY delete the specific named resources, nothing else
- Namespaced Role — cannot touch Prometheus in other namespaces
- Only deletes resources with names matching zxporter's Prometheus install (`prometheus-dz-*`, `dz-prometheus-*`, `prometheus-kube-state-*`)
- Customer's own Prometheus (in `monitoring` or any other namespace) is never affected
- `--ignore-not-found` on every delete — clean exit on fresh clusters

---

## What Stays the Same After Upgrade

| Item | Change? | Detail |
|------|---------|--------|
| Namespace | **No change** | Stays in `devzero-zxporter` or `devzero-system` — wherever it was |
| Cluster token | **No change** | Not touched, whether in Secret or ConfigMap |
| ConfigMap | **No change** | All settings preserved (excluded namespaces, log level, etc.) |
| Secret | **No change** | Token Secret preserved |
| Cluster identity on dashboard | **No change** | Same cluster, same history, same recommendations |
| DAKR operator | **No change** | Unaffected by zxporter upgrade |

---

## Manual Cleanup (if needed)

If the cleanup Job fails or you prefer to clean up manually:

```bash
# Set YOUR namespace (wherever zxporter is installed)
NS=devzero-system  # or devzero-zxporter

# Prometheus Server
kubectl delete deployment prometheus-dz-prometheus-server -n $NS --ignore-not-found
kubectl delete service prometheus-dz-prometheus-server -n $NS --ignore-not-found
kubectl delete serviceaccount prometheus-dz-prometheus-server -n $NS --ignore-not-found
kubectl delete configmap prometheus-dz-prometheus-server -n $NS --ignore-not-found
kubectl delete clusterrole prometheus-dz-prometheus-server --ignore-not-found
kubectl delete clusterrolebinding prometheus-dz-prometheus-server --ignore-not-found

# Kube-State-Metrics
kubectl delete deployment prometheus-kube-state-metrics -n $NS --ignore-not-found
kubectl delete service prometheus-kube-state-metrics -n $NS --ignore-not-found
kubectl delete serviceaccount prometheus-kube-state-metrics -n $NS --ignore-not-found
kubectl delete clusterrole prometheus-kube-state-metrics --ignore-not-found
kubectl delete clusterrolebinding prometheus-kube-state-metrics --ignore-not-found

# Node-Exporter
kubectl delete daemonset dz-prometheus-node-exporter -n $NS --ignore-not-found
kubectl delete service dz-prometheus-node-exporter -n $NS --ignore-not-found
kubectl delete serviceaccount dz-prometheus-node-exporter -n $NS --ignore-not-found

# Metrics-Server (only the one installed by zxporter)
kubectl delete deployment dz-metrics-server -n $NS --ignore-not-found
kubectl delete service dz-metrics-server -n $NS --ignore-not-found
kubectl delete serviceaccount dz-metrics-server -n $NS --ignore-not-found
kubectl delete clusterrole system:dz-metrics-server-aggregated-reader system:dz-metrics-server --ignore-not-found
kubectl delete clusterrolebinding dz-metrics-server:system:auth-delegator system:dz-metrics-server --ignore-not-found
kubectl delete rolebinding dz-metrics-server-auth-reader -n kube-system --ignore-not-found

# Standalone Nodemon (only if previously installed separately)
kubectl delete daemonset zxporter-nodemon -n $NS --ignore-not-found
kubectl delete serviceaccount zxporter-nodemon -n $NS --ignore-not-found
kubectl delete configmap zxporter-nodemon-dcgm-metrics zxporter-nodemon-zxporter-nodemon -n $NS --ignore-not-found
kubectl delete clusterrole zxporter-nodemon --ignore-not-found
kubectl delete clusterrolebinding zxporter-nodemon --ignore-not-found
```

---

## FAQ

**Q: Will this affect my other Prometheus installation?**
No. The cleanup only deletes resources by exact name (`prometheus-dz-prometheus-server`, etc.) in the zxporter namespace. Your Prometheus in `monitoring` or any other namespace is untouched.

**Q: My zxporter is in `devzero-zxporter`, will this work?**
Yes. The DAKR backend auto-detects the namespace. The cleanup Job runs in whatever namespace zxporter is installed in.

**Q: Do I need to change my cluster token?**
No. The token is not touched during upgrade.

**Q: What if the cleanup Job fails?**
The new zxporter works fine regardless — Prometheus resources just sit there unused. Clean up manually when convenient using the script above.

**Q: Can I still use `kubectl top` after the upgrade?**
If your cluster has a managed metrics-server (EKS, GKE, AKS all do), yes. If `kubectl top` breaks, check: `kubectl get apiservice v1beta1.metrics.k8s.io -o jsonpath='{.spec.service}'` — if it points to `dz-metrics-server`, patch it to point to your cluster's metrics-server: `kubectl patch apiservice v1beta1.metrics.k8s.io --type merge -p '{"spec":{"service":{"name":"metrics-server","namespace":"kube-system"}}}'`

**Q: What about the `PROMETHEUS_URL` in my old ConfigMap?**
Ignored. The new zxporter binary doesn't read it. You don't need to remove it — it's harmless.

**Q: What about `ENABLE_NODEMON_METRICS` in the ConfigMap?**
Ignored. The new zxporter always uses nodemon. Old ConfigMap values are harmless.
