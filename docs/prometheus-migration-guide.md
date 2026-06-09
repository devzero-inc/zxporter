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

## Option 4: Full Manual Migration (Step-by-Step)

Use this when the automated paths above don't work, or you want full control over every step. Follow in order — do NOT skip steps.

### Step 1: Find where zxporter is running

```bash
kubectl get deployment -A | grep zxporter-controller-manager
```

You will see something like:
```
devzero-zxporter   devzero-zxporter-controller-manager   2/2   2   2   90d
```

Set the namespace:
```bash
export NS=devzero-zxporter
```
> Replace with your actual namespace (`devzero-system`, `devzero-zxporter`, or whatever you see).

### Step 2: Save your cluster token

This is the **most important step**. If you lose the token, you will need to generate a new one from the DevZero dashboard.

**Try the Secret first** (most common for recent installs):
```bash
kubectl get secret devzero-zxporter-token -n $NS -o jsonpath='{.data.CLUSTER_TOKEN}' | base64 -d
```

**If that returns empty, try the ConfigMap:**
```bash
kubectl get configmap devzero-zxporter-env-config -n $NS -o jsonpath='{.data.CLUSTER_TOKEN}'
```

**If BOTH are empty, check if a PAT token was used:**
```bash
kubectl get secret devzero-zxporter-credentials -n $NS -o jsonpath='{.data.PAT_TOKEN}' | base64 -d
```

**Save whatever you find:**
```bash
export CLUSTER_TOKEN="dakr-xxxxx..."   # paste your cluster token here
# OR
export PAT_TOKEN="dzu-xxxxx..."        # paste your PAT token here (if no cluster token)
```

> **STOP HERE if you have neither a cluster token nor a PAT token.** Go to the DevZero dashboard, find your cluster, and generate a new token before continuing.

### Step 3: Save your current config

```bash
export DAKR_URL=$(kubectl get configmap devzero-zxporter-env-config -n $NS -o jsonpath='{.data.DAKR_URL}')
export CLUSTER_NAME=$(kubectl get configmap devzero-zxporter-env-config -n $NS -o jsonpath='{.data.KUBE_CONTEXT_NAME}')
export K8S_PROVIDER=$(kubectl get configmap devzero-zxporter-env-config -n $NS -o jsonpath='{.data.K8S_PROVIDER}')
export LOG_LEVEL=$(kubectl get configmap devzero-zxporter-env-config -n $NS -o jsonpath='{.data.LOG_LEVEL}')
```

**Verify you have everything:**
```bash
echo "Namespace:    $NS"
echo "Token:        ${CLUSTER_TOKEN:-(not set)}"
echo "PAT:          ${PAT_TOKEN:-(not set)}"
echo "DAKR URL:     $DAKR_URL"
echo "Cluster name: $CLUSTER_NAME"
echo "Provider:     $K8S_PROVIDER"
echo "Log level:    $LOG_LEVEL"
```

> **Check:** At minimum you need NS, a token (cluster or PAT), DAKR_URL, and CLUSTER_NAME. If any are missing, get them from your DevZero dashboard before continuing.

### Step 4: Delete the old zxporter entirely

We're going to delete everything — the deployment, Prometheus, nodemon, Helm releases, and the namespace itself. This is a clean-slate approach.

**4a: Uninstall Helm releases (if installed via Helm):**
```bash
helm uninstall zxporter -n $NS 2>/dev/null || true
helm uninstall zxporter-nodemon -n $NS 2>/dev/null || true
```

**4b: Delete all resources in the namespace:**
```bash
kubectl delete all --all -n $NS
kubectl delete configmap --all -n $NS
kubectl delete secret --all -n $NS
kubectl delete pdb --all -n $NS
kubectl delete role,rolebinding --all -n $NS
```

**4c: Delete cluster-scoped resources left by old zxporter:**
```bash
# ZXporter RBAC
kubectl delete clusterrole devzero-zxporter-collectionpolicy-editor-role --ignore-not-found
kubectl delete clusterrole devzero-zxporter-collectionpolicy-viewer-role --ignore-not-found
kubectl delete clusterrole devzero-zxporter-manager-role --ignore-not-found
kubectl delete clusterrole devzero-zxporter-metrics-auth-role --ignore-not-found
kubectl delete clusterrole devzero-zxporter-metrics-reader --ignore-not-found
kubectl delete clusterrolebinding devzero-zxporter-manager-rolebinding --ignore-not-found
kubectl delete clusterrolebinding devzero-zxporter-metrics-auth-rolebinding --ignore-not-found

# Prometheus RBAC
kubectl delete clusterrole prometheus-dz-prometheus-server --ignore-not-found
kubectl delete clusterrole prometheus-kube-state-metrics --ignore-not-found
kubectl delete clusterrolebinding prometheus-dz-prometheus-server --ignore-not-found
kubectl delete clusterrolebinding prometheus-kube-state-metrics --ignore-not-found

# Standalone nodemon RBAC
kubectl delete clusterrole zxporter-nodemon --ignore-not-found
kubectl delete clusterrolebinding zxporter-nodemon --ignore-not-found

# Metrics-server RBAC
kubectl delete clusterrole system:dz-metrics-server-aggregated-reader system:dz-metrics-server --ignore-not-found
kubectl delete clusterrolebinding dz-metrics-server:system:auth-delegator system:dz-metrics-server --ignore-not-found
kubectl delete rolebinding dz-metrics-server-auth-reader -n kube-system --ignore-not-found

# Priority class
kubectl delete priorityclass devzero-zxporter-devzero-zxporter-critical --ignore-not-found
```

**4d: Delete the namespace:**
```bash
kubectl delete namespace $NS
```

**4e: If the namespace is stuck in `Terminating` state:**

This happens when a stale APIService (like `v1beta1.metrics.k8s.io` pointing to the deleted `dz-metrics-server`) blocks namespace finalization.

Check if it's stuck:
```bash
kubectl get namespace $NS
# If it shows "Terminating" for more than 1 minute, it's stuck
```

Force-remove the finalizer:
```bash
kubectl get namespace $NS -o json \
  | jq '.spec.finalizers = []' \
  | kubectl replace --raw "/api/v1/namespaces/$NS/finalize" -f -
```

Wait and verify it's gone:
```bash
kubectl get namespace $NS 2>&1
# Should say: "Error from server (NotFound): namespaces "xxx" not found"
```

> **If it's STILL stuck:** The stale `v1beta1.metrics.k8s.io` APIService is the cause. Fix it first, then the namespace will finalize:
> ```bash
> # Check what it points to
> kubectl get apiservice v1beta1.metrics.k8s.io -o jsonpath='{.spec.service}'
>
> # If it points to dz-metrics-server (deleted), fix it:
> kubectl patch apiservice v1beta1.metrics.k8s.io --type merge \
>   -p '{"spec":{"service":{"name":"metrics-server","namespace":"kube-system","port":443}}}'
>
> # If your cluster has no metrics-server at all, delete the stale APIService:
> kubectl delete apiservice v1beta1.metrics.k8s.io
> ```
> After fixing the APIService, the stuck namespace should finalize within 30 seconds.

**4f: Verify the cluster is clean:**
```bash
# No zxporter namespaces
kubectl get ns | grep -iE "devzero|zxporter"
# Should return nothing

# No zxporter cluster resources
kubectl get clusterrole,clusterrolebinding | grep -iE "zxporter|prometheus-dz|prometheus-kube"
# Should return nothing
```

### Step 5: Install the new zxporter

Choose ONE of these sub-options:

#### 5a: Using Helm (recommended)

```bash
# If old Helm release exists, uninstall it first
helm uninstall zxporter -n $NS 2>/dev/null || true

# Update subchart dependency
helm dependency update ./helm-chart/zxporter/

# Install
helm install zxporter ./helm-chart/zxporter \
  --namespace $NS --create-namespace \
  --set zxporter.clusterToken="$CLUSTER_TOKEN" \
  --set zxporter.kubeContextName="$CLUSTER_NAME" \
  --set zxporter.k8sProvider="$K8S_PROVIDER" \
  --set zxporter.dakrUrl="$DAKR_URL" \
  --set zxporter.logLevel="${LOG_LEVEL:-error}" \
  --set zxporter-nodemon.provider="$K8S_PROVIDER"
```

> If using PAT token instead of cluster token, replace `--set zxporter.clusterToken=...` with `--set zxporter.patToken="$PAT_TOKEN"`.

> For clusters without GPUs, add: `--set zxporter.disableGPUMetrics=true`

#### 5b: Using kubectl apply (dist/install.yaml)

The `dist/install.yaml` defaults to `devzero-system` namespace. If your old install was in a **different namespace** (e.g., `devzero-zxporter`), you need to sed the namespace too.

**If installing into `devzero-system` (default):**
```bash
cat dist/install.yaml | sed \
  "s|CLUSTER_TOKEN: \"\"|CLUSTER_TOKEN: \"$CLUSTER_TOKEN\"|g" | sed \
  "s|DAKR_URL: https://dakr.devzero.io|DAKR_URL: $DAKR_URL|g" | sed \
  "s|KUBE_CONTEXT_NAME: '{{ .kube_context_name }}'|KUBE_CONTEXT_NAME: $CLUSTER_NAME|g" | sed \
  "s|K8S_PROVIDER: '{{ .k8s_provider }}'|K8S_PROVIDER: $K8S_PROVIDER|g" \
  | kubectl apply -f -
```

**If installing into a different namespace** (e.g., `devzero-zxporter`):
```bash
cat dist/install.yaml | sed \
  "s|CLUSTER_TOKEN: \"\"|CLUSTER_TOKEN: \"$CLUSTER_TOKEN\"|g" | sed \
  "s|DAKR_URL: https://dakr.devzero.io|DAKR_URL: $DAKR_URL|g" | sed \
  "s|KUBE_CONTEXT_NAME: '{{ .kube_context_name }}'|KUBE_CONTEXT_NAME: $CLUSTER_NAME|g" | sed \
  "s|K8S_PROVIDER: '{{ .k8s_provider }}'|K8S_PROVIDER: $K8S_PROVIDER|g" | sed \
  "s|namespace: devzero-system|namespace: $NS|g" | sed \
  "s|name: devzero-system|name: $NS|g" \
  | kubectl apply -f -
```

> **Note:** The install.yaml includes a Prometheus cleanup Job. Since you already deleted everything in Step 4, the Job will run, find nothing, exit cleanly, and self-delete after 5 minutes. This is harmless.

#### 5c: Using DAKR backend curl

```bash
curl -XPOST \
  -H "Authorization: Bearer $PAT_TOKEN" \
  -H "X-Kube-Context-Name: $CLUSTER_NAME" \
  "$DAKR_URL/dakr/installer-manifest?cluster-provider=$K8S_PROVIDER" \
  | kubectl apply -f -
```

### Step 6: Wait for pods to be ready

```bash
echo "Waiting for zxporter deployment..."
kubectl rollout status deployment/devzero-zxporter-controller-manager -n $NS --timeout=180s

echo "Waiting for nodemon daemonset..."
kubectl rollout status daemonset -l app.kubernetes.io/name=zxporter-nodemon -n $NS --timeout=180s 2>/dev/null \
  || kubectl rollout status daemonset -l app.kubernetes.io/name=zxporter-nodemon -n $NS --timeout=180s 2>/dev/null

echo "All pods:"
kubectl get pods -n $NS
```

Expected output:
```
devzero-zxporter-controller-manager-xxx   1/1   Running   0   30s
devzero-zxporter-controller-manager-yyy   1/1   Running   0   30s
zxporter-zxporter-nodemon-aaa             2/2   Running   0   30s   (one per node)
zxporter-zxporter-nodemon-bbb             2/2   Running   0   30s
...
```

> **If nodemon shows 0/2 or CrashLoopBackOff:** Check `kubectl describe pod <nodemon-pod> -n $NS` — the most common issue is missing ConfigMaps. Reapply the manifest.

> **If zxporter shows 0/1:** Check `kubectl logs deployment/devzero-zxporter-controller-manager -n $NS --tail=20` — look for auth errors (bad token) or connection errors (wrong DAKR URL).

### Step 7: Verify data is flowing

**Check zxporter logs for successful sends:**
```bash
kubectl logs deployment/devzero-zxporter-controller-manager -n $NS --tail=30 \
  | grep -E "Successfully sent|container_resource|node_resource|error" \
  | tail -10
```

You should see lines like:
```
Successfully sent batch  batchSize: 80
Splitting resources into batches  resourceType: container_resource
Splitting resources into batches  resourceType: node_resource
```

> **If you see `"error"` lines**, read them carefully:
> - `"invalid token"` → your cluster token is wrong. Get a new one from the dashboard.
> - `"connection refused"` → wrong DAKR URL. Check `$DAKR_URL`.
> - `"Failed to fetch container metrics from nodemon"` → nodemon pods aren't ready yet. Wait 30 seconds and check again.
> - `"nodemon returned status 404"` → nodemon image is old. Make sure you're using the new nodemon image that has `/v2/container/metrics`.

**Check no Prometheus errors:**
```bash
kubectl logs deployment/devzero-zxporter-controller-manager -n $NS --tail=50 \
  | grep -i "prometheus"
```
> This should return nothing. If it shows Prometheus connection errors, the new binary isn't installed correctly.

**Check the nodemon is serving data:**
```bash
NODEMON_IP=$(kubectl get pods -n $NS -l app.kubernetes.io/name=zxporter-nodemon \
  -o jsonpath='{.items[0].status.podIP}')

kubectl run migration-verify --rm -i --restart=Never --image=curlimages/curl -n $NS \
  -- curl -s "http://$NODEMON_IP:6061/v2/container/metrics" | head -c 300
```

You should see JSON with `cpu_usage_nanocores`, `memory_working_set_bytes`, etc.

### Step 8: Verify on the dashboard

Open the DevZero dashboard and check your cluster:

1. **Cluster overview** — CPU and Memory utilization graphs should show data within 2 minutes
2. **Workloads** — CPU/Memory usage columns should have non-zero values
3. **Nodes** — Node metrics should show network and disk I/O

> **If the dashboard shows no data:** Wait 5 minutes (data needs to be ingested and processed). If still nothing after 5 minutes, go back to Step 7 and check the logs.

### Step 9: Update DAKR operator zxporter-addr (if namespace changed)

If your new zxporter is in a **different namespace** than the old one (e.g., old was in `devzero-zxporter`, new is in `devzero-system`), the DAKR operator's MPA connector still points to the old namespace and won't be able to connect to zxporter's MPA gRPC service.

**Check the current setting:**
```bash
kubectl get deployment -n dakr-operator -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' \
  -l app.kubernetes.io/name=dakr-operator | tr ',' '\n' | grep zxporter-addr
```

Example output:
```
"--zxporter-addr=devzero-zxporter-controller-manager-mpa.devzero-zxporter.svc.cluster.local:50051"
```

**If the namespace in the URL doesn't match where you just installed zxporter**, update it.

The correct format is:
```
devzero-zxporter-controller-manager-mpa.<YOUR_ZXPORTER_NAMESPACE>.svc.cluster.local:50051
```

**Verify which namespace has the MPA service:**
```bash
kubectl get service -A | grep mpa
```

**Update via Helm** (replace `<NEW_NS>` with your zxporter namespace):
```bash
helm upgrade dakr <your-dakr-chart> \
  --namespace dakr-operator \
  --reuse-values \
  --set operator.zxporterAddr="devzero-zxporter-controller-manager-mpa.<NEW_NS>.svc.cluster.local:50051"
```

**Verify the operator connects to zxporter MPA:**
```bash
# Wait for rollout
kubectl rollout status deployment -n dakr-operator -l app.kubernetes.io/name=dakr-operator --timeout=120s

# Check logs for MPA activity
kubectl logs deployment/$(kubectl get deployment -n dakr-operator -l app.kubernetes.io/name=dakr-operator -o name | head -1 | cut -d/ -f2) \
  -n dakr-operator --tail=20 | grep -iE "mpa|rule.*eval|metrics.*batch"
```

You should see lines like:
```
Initializing Rule Evaluator Controller (unified MPA)
Received metrics batch  item_count: 5
HPA engine decision  workload: my-app  desired_replicas: 2
```

> **If you see `Rule Evaluator Controller is disabled`**, add `--set mpaController.enabled=true` to the Helm upgrade command.

> **If the namespace didn't change** (e.g., old and new are both in `devzero-system`), skip this step — the address is already correct.

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

## Rollback

If something goes wrong and you need to go back to the old version:

**Helm:**
```bash
helm rollback zxporter -n $NS
```

**kubectl:**
```bash
# Scale down new zxporter
kubectl scale deployment devzero-zxporter-controller-manager -n $NS --replicas=0

# Re-apply the old installer from the DAKR backend
curl -XPOST -H 'Authorization: Bearer <YOUR_PAT>' \
  "https://dakr.devzero.io/dakr/installer-updater" \
  | kubectl apply -f -
```

> The old Prometheus components won't come back automatically. If you need them, re-apply the old `dist/install.yaml` from a previous release.

---

## FAQ

**Q: Will this affect my other Prometheus installation?**
No. The cleanup only deletes resources by exact name (`prometheus-dz-prometheus-server`, etc.) in the zxporter namespace. Your Prometheus in `monitoring` or any other namespace is untouched.

**Q: My zxporter is in `devzero-zxporter`, will this work?**
Yes. The DAKR backend auto-detects the namespace. The cleanup Job runs in whatever namespace zxporter is installed in.

**Q: Do I need to change my cluster token?**
No. The token is not touched during upgrade.

**Q: What if the cleanup Job fails?**
The new zxporter works fine regardless — Prometheus resources just sit there unused. Clean up manually when convenient using the commands in Step 4 above.

**Q: Can I still use `kubectl top` after the upgrade?**
If your cluster has a managed metrics-server (EKS, GKE, AKS all do), yes. If `kubectl top` breaks, see Step 4e above.

**Q: What about the `PROMETHEUS_URL` in my old ConfigMap?**
Ignored. The new zxporter binary doesn't read it. You don't need to remove it — it's harmless.

**Q: What about `ENABLE_NODEMON_METRICS` in the ConfigMap?**
Ignored. The new zxporter always uses nodemon. Old ConfigMap values are harmless.

**Q: How long is the data gap during manual migration?**
From when you delete everything in Step 4 to when new pods start sending in Step 6 — typically 2-5 minutes. The dashboard will show a brief gap. Historical data is not affected.

**Q: What if I have both a cluster token and a PAT token?**
Use the cluster token. PAT tokens are exchanged for cluster tokens on startup — if you already have a cluster token, use it directly.

**Q: I accidentally deleted the ConfigMap/Secret. What do I do?**
The new install in Step 5 creates them fresh. You just need the cluster token (or PAT) to put in the new install command. If you lost both, generate a new token from the DevZero dashboard.

**Q: The nodemon pods show `ImagePullBackOff`.**
The nodemon image isn't accessible. Check `kubectl describe pod <pod> -n $NS | grep Image` — make sure the image repository and tag are correct and accessible from your cluster.

**Q: I see `Collector not available, skipping registration` for `container_resource` in the logs.**
The nodemon client couldn't discover nodemon pods. Make sure nodemon pods are running in the **same namespace** as zxporter: `kubectl get pods -n $NS -l app.kubernetes.io/name=zxporter-nodemon`
