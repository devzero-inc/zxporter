# Prometheus Removal Migration Guide

## What Changed

The new zxporter collects all metrics via the **nodemon DaemonSet** (polling kubelet directly) instead of Prometheus. When you upgrade zxporter, the old Prometheus components become unused and should be cleaned up.

**What gets added:** nodemon DaemonSet (bundled with zxporter)
**What gets removed:** Prometheus server, kube-state-metrics, node-exporter, dz-metrics-server

**What does NOT change:** your cluster token, your cluster identity on the dashboard.

---

## How to Upgrade

### Option 1: Automated (DAKR backend updater)

```bash
curl -XPOST -H 'Authorization: Bearer <YOUR_PAT>' \
  "https://dakr.devzero.io/dakr/installer-updater" \
  | kubectl apply -f -
```

The updater auto-detects namespace, preserves ConfigMap/Secret, deploys nodemon, and runs cleanup.

### Option 2: Helm upgrade

```bash
NS=$(helm list -A | grep zxporter | awk '{print $2}')
helm upgrade zxporter <chart-source> --namespace $NS --reuse-values
```

### Option 3: Fresh install (new cluster)

```bash
curl -XPOST -H 'Authorization: Bearer <YOUR_PAT>' \
  -H "X-Kube-Context-Name: $(kubectl config current-context)" \
  "https://dakr.devzero.io/dakr/installer-manifest?cluster-provider=AWS" \
  | kubectl apply -f -
```

---

## Option 4: Full Manual Migration (Step-by-Step)

This is for when you want full control, or the automated paths don't work. Every step explains **what is happening** and **why**. Follow in order.

> **Time required:** 10-15 minutes
> **Data gap:** ~2-5 minutes (between deleting old and new pods sending data)
> **Risk:** Zero if you save the token correctly in Step 2

---

### Step 1: Download the new zxporter manifest

Get the install command from the DevZero UI. It looks like this:

```bash
curl -XPOST \
  -H "Authorization: Bearer <YOUR_PAT_OR_BEARER_TOKEN>" \
  -H "X-Kube-Context-Name: <CLUSTER_NAME>" \
  "<DAKR_URL>/dakr/installer-manifest?cluster-provider=<PROVIDER>" \
  | kubectl apply -f -
```

**Do NOT run this yet.** Instead, save the manifest to a file for inspection:

```bash
curl -XPOST \
  -H "Authorization: Bearer <YOUR_PAT_OR_BEARER_TOKEN>" \
  -H "X-Kube-Context-Name: <CLUSTER_NAME>" \
  "<DAKR_URL>/dakr/installer-manifest?cluster-provider=<PROVIDER>" \
  > /tmp/new-zxporter.yaml
```

> **What is happening:** We're downloading the new manifest but NOT applying it yet. We'll inspect and patch it first.

**Verify the file is valid:**
```bash
wc -l /tmp/new-zxporter.yaml
# Should be 500+ lines. If it's 0 or just an error message, the curl failed.
# Check: cat /tmp/new-zxporter.yaml | head -20
```

---

### Step 2: Backup your current cluster config

Find where the old zxporter is running and save ALL the config we need.

**2a: Find the namespace:**
```bash
export OLD_NS=$(kubectl get deployment -A -l control-plane=controller-manager \
  -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null)
echo "Old zxporter namespace: $OLD_NS"
```

> If this returns empty, try: `kubectl get deployment -A | grep zxporter`

**2b: Save the cluster token** (MOST IMPORTANT — do not lose this):
```bash
# Try Secret first (most common)
export CLUSTER_TOKEN=$(kubectl get secret devzero-zxporter-token -n $OLD_NS \
  -o jsonpath='{.data.CLUSTER_TOKEN}' 2>/dev/null | base64 -d)

# If empty, try ConfigMap
if [ -z "$CLUSTER_TOKEN" ]; then
  export CLUSTER_TOKEN=$(kubectl get configmap devzero-zxporter-env-config -n $OLD_NS \
    -o jsonpath='{.data.CLUSTER_TOKEN}' 2>/dev/null)
fi

# If still empty, try PAT token
if [ -z "$CLUSTER_TOKEN" ]; then
  export PAT_TOKEN=$(kubectl get secret devzero-zxporter-credentials -n $OLD_NS \
    -o jsonpath='{.data.PAT_TOKEN}' 2>/dev/null | base64 -d)
fi

echo "Cluster token: ${CLUSTER_TOKEN:-(not found)}"
echo "PAT token:     ${PAT_TOKEN:-(not found)}"
```

**2c: Save other config values:**
```bash
export DAKR_URL=$(kubectl get configmap devzero-zxporter-env-config -n $OLD_NS \
  -o jsonpath='{.data.DAKR_URL}')
export CLUSTER_NAME=$(kubectl get configmap devzero-zxporter-env-config -n $OLD_NS \
  -o jsonpath='{.data.KUBE_CONTEXT_NAME}')
export K8S_PROVIDER=$(kubectl get configmap devzero-zxporter-env-config -n $OLD_NS \
  -o jsonpath='{.data.K8S_PROVIDER}')
export LOG_LEVEL=$(kubectl get configmap devzero-zxporter-env-config -n $OLD_NS \
  -o jsonpath='{.data.LOG_LEVEL}')
```

**2d: Print everything and verify:**
```bash
echo "========================================="
echo " BACKUP COMPLETE — VERIFY BEFORE CONTINUING"
echo "========================================="
echo "Old namespace:  $OLD_NS"
echo "Cluster token:  ${CLUSTER_TOKEN:-(NOT SET)}"
echo "PAT token:      ${PAT_TOKEN:-(NOT SET)}"
echo "DAKR URL:       $DAKR_URL"
echo "Cluster name:   $CLUSTER_NAME"
echo "Provider:       $K8S_PROVIDER"
echo "Log level:      ${LOG_LEVEL:-(default)}"
echo "========================================="
echo ""
echo "CHECK: Do you have at least a token (cluster or PAT), DAKR URL, and cluster name?"
echo "If anything critical is missing, DO NOT continue. Get it from the DevZero dashboard."
```

> **What is happening:** We're saving everything we need to configure the new install. If we mess up later, we can start over with these values.

---

### Step 3: Validate the new manifest

The `installer_updater.yaml` does NOT contain the ConfigMap or Secret (those are managed separately — see Step 4d and 5b). It only contains the Deployment, RBAC, nodemon DaemonSet, and cleanup Job. Verify the things that ARE in it:

```bash
echo "=== Checking manifest ==="
echo ""
echo "Namespace used:"
grep "namespace:" /tmp/new-zxporter.yaml | sort -u
echo ""
echo "Images:"
grep "image:" /tmp/new-zxporter.yaml
echo ""
echo "Resource kinds:"
grep "^kind:" /tmp/new-zxporter.yaml | sort | uniq -c
```

**Check these things:**

| Field | What to look for | Problem if wrong |
|-------|-----------------|------------------|
| namespace | Should be `devzero-system` (we'll standardize). If it says `devzero-zxporter`, fix in 3a. | Resources go to wrong namespace |
| image | Should be the new zxporter/nodemon version, NOT `ttl.sh` | Pods won't start |
| kinds | Should include Deployment, DaemonSet, ServiceAccount, ClusterRole, etc. | Manifest is incomplete |

> **Note:** You will NOT see `CLUSTER_TOKEN`, `DAKR_URL`, `K8S_PROVIDER`, or `KUBE_CONTEXT_NAME` in this file. Those live in the ConfigMap and Secret which we export in Step 4d and restore in Step 5b.

**3a: Fix the namespace (if needed):**

If the manifest says `devzero-zxporter` but you want `devzero-system`:

```bash
# Change namespace: field on all resources
sed -i '' "s|namespace: devzero-zxporter|namespace: devzero-system|g" /tmp/new-zxporter.yaml

# Change the Namespace resource's own name (the $ ensures we don't match
# resource names like devzero-zxporter-controller-manager)
sed -i '' "s|name: devzero-zxporter$|name: devzero-system|g" /tmp/new-zxporter.yaml

# Verify
grep "namespace:" /tmp/new-zxporter.yaml | sort -u
# Should all say devzero-system now
```

> **What is happening:** We're making sure the manifest targets the right namespace and has the right images before applying. ConfigMap/Secret values are handled separately in Steps 4d and 5b.

**3b: Adjust zxporter resource requests based on cluster size:**

The default resource requests (`cpu: 200m`, `memory: 128Mi`) are for small clusters. For larger clusters, zxporter processes more pods/nodes per cycle and needs more resources. Check your cluster size and adjust:

```bash
echo "Cluster size:"
echo "  Nodes: $(kubectl get nodes --no-headers | wc -l)"
echo "  Pods:  $(kubectl get pods -A --no-headers | wc -l)"
```

Use this table to pick the right resources:

| Cluster Size | Nodes | Pods | CPU Request | Memory Request | CPU Limit | Memory Limit |
|---|---|---|---|---|---|---|
| **Small** | 1-10 | < 100 | 100m | 128Mi | 200m | 256Mi |
| **Medium** | 10-50 | 100-500 | 200m | 256Mi | 400m | 512Mi |
| **Large** | 50-200 | 500-2000 | 300m | 512Mi | 600m | 1Gi |
| **XL** | 200+ | 2000+ | 500m | 1Gi | 1000m | 2Gi |

> **Why it matters:** Each collection cycle, zxporter queries nodemon for every node, iterates every pod, and builds snapshots. With 2000 pods, a single cycle processes 2000+ container metrics. Undersized zxporter will be slow (data arrives late) or OOM-killed.

> **Nodemon is fine** — it only collects metrics from its own node, so resource usage is constant regardless of cluster size. The defaults (100m CPU, 128Mi memory) work for all sizes.

**To patch the resources in the manifest:**
```bash
# Check the controller-manager Deployment's container resources
# (grep "resources:" alone matches RBAC rules — isolate the Deployment block first)
awk '/kind: Deployment/,/^---/' /tmp/new-zxporter.yaml | grep -A4 "resources:"

# For a Medium cluster (10-50 nodes), patch to 200m/256Mi:
# The easiest way is to edit the file directly:
# vim /tmp/new-zxporter.yaml    (search for "name: manager", then find "resources:" below it)
# OR use yq:
# yq eval '(select(.kind == "Deployment") | .spec.template.spec.containers[0].resources.requests.cpu) = "200m"' -i /tmp/new-zxporter.yaml
# yq eval '(select(.kind == "Deployment") | .spec.template.spec.containers[0].resources.requests.memory) = "256Mi"' -i /tmp/new-zxporter.yaml
```

**For Helm installs** (Step 5a), pass resources directly:
```bash
--set resources.requests.cpu=200m \
--set resources.requests.memory=256Mi \
--set resources.limits.cpu=400m \
--set resources.limits.memory=512Mi
```

> **If you're unsure,** start with Medium and monitor. You can always adjust later with `kubectl edit deployment devzero-zxporter-controller-manager -n devzero-system` or a Helm upgrade.

---

### Step 4: Export config and clean up old zxporter

> **WARNING: Read this before doing anything.**
> - If old zxporter is in `devzero-zxporter` (or any **dedicated** namespace) → we delete the ENTIRE namespace (safe)
> - If old zxporter is in `devzero-system` → we delete ONLY zxporter resources by name (**DO NOT** `delete all --all` — DAKR operator and other components live here too)

**4a: Export ConfigMap and Secret FIRST (before deleting anything):**

> **CRITICAL:** The `installer_updater.yaml` does NOT include the ConfigMap or Secret. If we delete them, zxporter can't authenticate. Export them now.

```bash
export NEW_NS=devzero-system

echo "=== Exporting ConfigMap and Secret from $OLD_NS ==="

# Export ConfigMap (strip metadata that would cause conflicts)
kubectl get configmap devzero-zxporter-env-config -n $OLD_NS -o yaml \
  | grep -v "resourceVersion\|uid\|creationTimestamp\|selfLink\|namespace:" \
  | sed "s|^  name:|  namespace: $NEW_NS\n  name:|" \
  > /tmp/zxporter-configmap.yaml
echo "ConfigMap saved to /tmp/zxporter-configmap.yaml"

# Export Secret (cluster token) — may not exist on older installs where token is in ConfigMap
kubectl get secret devzero-zxporter-token -n $OLD_NS -o yaml 2>/dev/null \
  | grep -v "resourceVersion\|uid\|creationTimestamp\|selfLink\|namespace:" \
  | sed "s|^  name:|  namespace: $NEW_NS\n  name:|" \
  > /tmp/zxporter-secret.yaml 2>/dev/null
if [ -s /tmp/zxporter-secret.yaml ]; then
  echo "Secret saved to /tmp/zxporter-secret.yaml"
else
  echo "No token Secret found (token is likely in ConfigMap — this is fine for older installs)"
  rm -f /tmp/zxporter-secret.yaml
fi

# Also try to export credentials secret (PAT token) if it exists
kubectl get secret devzero-zxporter-credentials -n $OLD_NS -o yaml 2>/dev/null \
  | grep -v "resourceVersion\|uid\|creationTimestamp\|selfLink\|namespace:" \
  | sed "s|^  name:|  namespace: $NEW_NS\n  name:|" \
  > /tmp/zxporter-credentials.yaml 2>/dev/null
if [ -s /tmp/zxporter-credentials.yaml ]; then
  echo "Credentials Secret saved to /tmp/zxporter-credentials.yaml"
else
  echo "No credentials Secret found (using cluster token directly — this is fine)"
  rm -f /tmp/zxporter-credentials.yaml
fi
```

**Verify the exports:**
```bash
echo "=== ConfigMap values ==="
grep -E "DAKR_URL|CLUSTER_TOKEN|KUBE_CONTEXT|K8S_PROVIDER|LOG_LEVEL" /tmp/zxporter-configmap.yaml
echo ""
echo "=== Secret ==="
if [ -f /tmp/zxporter-secret.yaml ]; then
  grep "CLUSTER_TOKEN" /tmp/zxporter-secret.yaml
else
  echo "No Secret file (token is in ConfigMap)"
fi
```

> **STOP if the exports are empty or broken.** You saved the values in Step 2 as fallback.

**4b: Uninstall Helm releases (if installed via Helm):**
```bash
helm uninstall zxporter -n $OLD_NS 2>/dev/null || true
helm uninstall zxporter-nodemon -n $OLD_NS 2>/dev/null || true
echo "Helm releases uninstalled (if they existed)"
```

**4c: Delete cluster-scoped resources (same for both paths):**
```bash
echo "Deleting cluster-scoped resources..."

# ZXporter RBAC
for r in devzero-zxporter-collectionpolicy-editor-role devzero-zxporter-collectionpolicy-viewer-role \
  devzero-zxporter-manager-role devzero-zxporter-metrics-auth-role devzero-zxporter-metrics-reader; do
  kubectl delete clusterrole "$r" --ignore-not-found
done
for r in devzero-zxporter-manager-rolebinding devzero-zxporter-metrics-auth-rolebinding; do
  kubectl delete clusterrolebinding "$r" --ignore-not-found
done

# Prometheus RBAC
for r in prometheus-dz-prometheus-server prometheus-kube-state-metrics; do
  kubectl delete clusterrole "$r" --ignore-not-found
  kubectl delete clusterrolebinding "$r" --ignore-not-found
done

# Nodemon RBAC
kubectl delete clusterrole zxporter-nodemon --ignore-not-found
kubectl delete clusterrolebinding zxporter-nodemon --ignore-not-found

# Metrics-server RBAC
kubectl delete clusterrole system:dz-metrics-server-aggregated-reader system:dz-metrics-server --ignore-not-found
kubectl delete clusterrolebinding dz-metrics-server:system:auth-delegator system:dz-metrics-server --ignore-not-found
kubectl delete rolebinding dz-metrics-server-auth-reader -n kube-system --ignore-not-found

# Priority class
kubectl delete priorityclass devzero-zxporter-devzero-zxporter-critical --ignore-not-found

echo "Cluster resources deleted"
```

Now pick **ONE** of the two paths below:

---

#### 4d PATH A — Old namespace is `devzero-zxporter` (or any dedicated zxporter-only namespace)

Safe to delete the entire namespace. Nothing else lives there.

```bash
echo "Deleting everything in $OLD_NS (dedicated zxporter namespace)..."
kubectl delete all --all -n $OLD_NS 2>/dev/null
kubectl delete configmap --all -n $OLD_NS 2>/dev/null
kubectl delete secret --all -n $OLD_NS 2>/dev/null
kubectl delete pdb --all -n $OLD_NS 2>/dev/null
kubectl delete role,rolebinding --all -n $OLD_NS 2>/dev/null
kubectl delete namespace $OLD_NS 2>/dev/null
echo "Waiting for namespace deletion..."
sleep 5
```

If the namespace is stuck in `Terminating`:
```bash
kubectl get namespace $OLD_NS 2>&1
# If it says "Terminating" for more than 1 minute, force-remove the finalizer:
kubectl get namespace $OLD_NS -o json \
  | jq '.spec.finalizers = []' \
  | kubectl replace --raw "/api/v1/namespaces/$OLD_NS/finalize" -f -

# If STILL stuck, a stale metrics APIService is blocking it:
kubectl get apiservice v1beta1.metrics.k8s.io -o jsonpath='{.spec.service}' 2>/dev/null
# If it points to dz-metrics-server (deleted), fix it:
kubectl patch apiservice v1beta1.metrics.k8s.io --type merge \
  -p '{"spec":{"service":{"name":"metrics-server","namespace":"kube-system","port":443}}}'
# Or if no metrics-server exists at all:
kubectl delete apiservice v1beta1.metrics.k8s.io 2>/dev/null
```

---

#### 4d PATH B — Old namespace is `devzero-system` (shared namespace)

> **DO NOT run `kubectl delete all --all -n devzero-system` — this will destroy the DAKR operator and every other component in the namespace.**

Delete ONLY zxporter-specific resources by name:

```bash
echo "Deleting ONLY zxporter resources from $OLD_NS (keeping DAKR operator and other components)..."

# ZXporter controller manager
kubectl delete deployment devzero-zxporter-controller-manager -n $OLD_NS --ignore-not-found
kubectl delete service devzero-zxporter-controller-manager-metrics-service -n $OLD_NS --ignore-not-found
kubectl delete service devzero-zxporter-controller-manager-mpa -n $OLD_NS --ignore-not-found
kubectl delete serviceaccount devzero-zxporter-controller-manager -n $OLD_NS --ignore-not-found
kubectl delete pdb devzero-zxporter-controller-manager -n $OLD_NS --ignore-not-found

# ZXporter ConfigMap and Secrets (already exported in 4a)
kubectl delete configmap devzero-zxporter-env-config -n $OLD_NS --ignore-not-found
kubectl delete secret devzero-zxporter-token -n $OLD_NS --ignore-not-found
kubectl delete secret devzero-zxporter-credentials -n $OLD_NS --ignore-not-found

# Roles and RoleBindings
kubectl delete role devzero-zxporter-leader-election-role -n $OLD_NS --ignore-not-found
kubectl delete role devzero-zxporter-metrics-auth-role -n $OLD_NS --ignore-not-found
kubectl delete rolebinding devzero-zxporter-leader-election-rolebinding -n $OLD_NS --ignore-not-found
kubectl delete rolebinding devzero-zxporter-metrics-auth-rolebinding -n $OLD_NS --ignore-not-found

# Nodemon
kubectl delete daemonset -l app.kubernetes.io/name=zxporter-nodemon -n $OLD_NS --ignore-not-found
kubectl delete serviceaccount zxporter-nodemon -n $OLD_NS --ignore-not-found
kubectl delete configmap -l app.kubernetes.io/name=zxporter-nodemon -n $OLD_NS --ignore-not-found

# Prometheus server
kubectl delete deployment prometheus-dz-prometheus-server -n $OLD_NS --ignore-not-found
kubectl delete service prometheus-dz-prometheus-server -n $OLD_NS --ignore-not-found
kubectl delete serviceaccount prometheus-dz-prometheus-server -n $OLD_NS --ignore-not-found
kubectl delete configmap prometheus-dz-prometheus-server -n $OLD_NS --ignore-not-found

# Kube-state-metrics
kubectl delete deployment prometheus-kube-state-metrics -n $OLD_NS --ignore-not-found
kubectl delete service prometheus-kube-state-metrics -n $OLD_NS --ignore-not-found
kubectl delete serviceaccount prometheus-kube-state-metrics -n $OLD_NS --ignore-not-found

# Node-exporter
kubectl delete daemonset dz-prometheus-node-exporter -n $OLD_NS --ignore-not-found
kubectl delete service dz-prometheus-node-exporter -n $OLD_NS --ignore-not-found
kubectl delete serviceaccount dz-prometheus-node-exporter -n $OLD_NS --ignore-not-found

# Metrics-server
kubectl delete deployment dz-metrics-server -n $OLD_NS --ignore-not-found
kubectl delete service dz-metrics-server -n $OLD_NS --ignore-not-found
kubectl delete serviceaccount dz-metrics-server -n $OLD_NS --ignore-not-found

echo "ZXporter resources deleted. Other components in $OLD_NS are untouched."
```

> **What is happening (PATH B):** We surgically removed only zxporter and its Prometheus components. The DAKR operator, any other deployments, and the namespace itself are left intact.

---

**4e: Verify cleanup:**
```bash
echo "=== Verification ==="
echo ""
echo "ZXporter resources remaining (should be empty):"
kubectl get all -n ${OLD_NS} 2>/dev/null | grep -iE "zxporter|prometheus-dz|prometheus-kube|dz-metrics|node-exporter" || echo "  (none — good)"
echo ""
echo "Cluster-scoped resources (should be empty):"
kubectl get clusterrole,clusterrolebinding | grep -iE "zxporter|prometheus-dz|prometheus-kube" || echo "  (none — good)"
echo ""
if [ "$OLD_NS" != "$NEW_NS" ]; then
  echo "Old namespace (should be gone if PATH A):"
  kubectl get ns $OLD_NS 2>&1 | grep -v "^NAME" || true
fi
echo ""
echo "Ready to install new zxporter!"
```

---

### Step 5: Install new zxporter

Choose **ONE** of these two install methods:

---

#### 5-A: kubectl apply (recommended for this migration)

**5a-1: Create the new namespace:**
```bash
kubectl create namespace $NEW_NS 2>/dev/null || true
echo "Namespace $NEW_NS ready"
```

**5a-2: Restore ConfigMap and Secret into the new namespace:**

The `installer_updater.yaml` does NOT include ConfigMap or Secret — zxporter needs them to exist before it starts.

```bash
echo "Restoring ConfigMap..."
kubectl apply -f /tmp/zxporter-configmap.yaml
echo ""

# Restore Secret (if it was exported — older installs may not have one)
if [ -f /tmp/zxporter-secret.yaml ]; then
  echo "Restoring Secret..."
  kubectl apply -f /tmp/zxporter-secret.yaml
else
  echo "No token Secret to restore (token is in ConfigMap)"
  # Create a Secret from the ConfigMap's token value for the new Secret-based approach:
  echo "Creating token Secret from ConfigMap value..."
  BACKUP_TOKEN=$(grep "CLUSTER_TOKEN:" /tmp/zxporter-configmap.yaml | head -1 | awk '{print $2}' | tr -d '"')
  if [ -n "$BACKUP_TOKEN" ]; then
    kubectl create secret generic devzero-zxporter-token -n $NEW_NS \
      --from-literal=CLUSTER_TOKEN="$BACKUP_TOKEN"
    echo "Token Secret created from ConfigMap value"
  else
    echo "WARNING: No token found in ConfigMap either. Use the value from Step 2:"
    echo "  kubectl create secret generic devzero-zxporter-token -n $NEW_NS --from-literal=CLUSTER_TOKEN=\"\$CLUSTER_TOKEN\""
  fi
fi
echo ""

# Restore credentials secret if it was exported
if [ -f /tmp/zxporter-credentials.yaml ]; then
  echo "Restoring Credentials Secret..."
  kubectl apply -f /tmp/zxporter-credentials.yaml
fi
```

**Verify they're in the right namespace:**
```bash
echo "ConfigMap:"
kubectl get configmap devzero-zxporter-env-config -n $NEW_NS
echo ""
echo "Secret:"
kubectl get secret devzero-zxporter-token -n $NEW_NS
echo ""
echo "Token value (should not be empty):"
kubectl get secret devzero-zxporter-token -n $NEW_NS -o jsonpath='{.data.CLUSTER_TOKEN}' | base64 -d
```

> **If ConfigMap or Secret is missing:** Recreate them manually from the values you saved in Step 2:
> ```bash
> kubectl create configmap devzero-zxporter-env-config -n $NEW_NS \
>   --from-literal=DAKR_URL="$DAKR_URL" \
>   --from-literal=KUBE_CONTEXT_NAME="$CLUSTER_NAME" \
>   --from-literal=K8S_PROVIDER="$K8S_PROVIDER" \
>   --from-literal=LOG_LEVEL="${LOG_LEVEL:-error}" \
>   --from-literal=USE_SECRET_FOR_TOKEN="true" \
>   --from-literal=TOKEN_CONFIGMAP_NAME="devzero-zxporter-env-config" \
>   --from-literal=TOKEN_SECRET_NAME="devzero-zxporter-token" \
>   --from-literal=TOKEN_CREDENTIALS_SECRET_NAME="devzero-zxporter-credentials" \
>   --from-literal=TOKEN_RUNTIME_SECRET_NAME="devzero-zxporter-token"
>
> kubectl create secret generic devzero-zxporter-token -n $NEW_NS \
>   --from-literal=CLUSTER_TOKEN="$CLUSTER_TOKEN"
> ```

**5a-3: Apply the manifest:**
```bash
kubectl apply -f /tmp/new-zxporter.yaml
```

> **What is happening:** The manifest creates RBAC, zxporter Deployment (2 replicas), nodemon DaemonSet (1 pod per node), and a Prometheus cleanup Job (harmless). The ConfigMap and Secret we restored in 5a-2 provide the token and config that zxporter reads on startup.

---

#### 5-B: Helm install

> **IMPORTANT: Do NOT restore ConfigMap/Secret before Helm install.**
> The Helm chart creates its own ConfigMap and Secret from the values you pass via `--set`.
> If you restore them first with `kubectl apply`, Helm will either:
> - **Fail** with "resource already exists" (the resources aren't Helm-managed), or
> - **Overwrite** them with empty defaults (losing your token)
>
> Instead, pass all values directly via `--set`:

```bash
helm dependency update ./helm-chart/zxporter/

helm install zxporter ./helm-chart/zxporter \
  --namespace $NEW_NS --create-namespace \
  --set zxporter.clusterToken="$CLUSTER_TOKEN" \
  --set zxporter.kubeContextName="$CLUSTER_NAME" \
  --set zxporter.k8sProvider="$K8S_PROVIDER" \
  --set zxporter.dakrUrl="$DAKR_URL" \
  --set zxporter.logLevel="${LOG_LEVEL:-error}" \
  --set zxporter-nodemon.provider="$K8S_PROVIDER"
```

> This creates everything in one shot: namespace, ConfigMap, Secret, Deployment, nodemon DaemonSet. Helm manages all resources.

> **If using PAT token instead of cluster token:** replace `--set zxporter.clusterToken=...` with `--set zxporter.patToken="$PAT_TOKEN"`

> **For clusters without GPUs:** add `--set zxporter.disableGPUMetrics=true`

---

### Step 6: Wait for everything to come up

```bash
export NS=devzero-system  # the new namespace

echo "Waiting for zxporter..."
kubectl rollout status deployment/devzero-zxporter-controller-manager -n $NS --timeout=180s

echo "Waiting for nodemon..."
kubectl rollout status daemonset -l app.kubernetes.io/name=zxporter-nodemon -n $NS --timeout=180s 2>/dev/null || true

echo ""
echo "=== All pods ==="
kubectl get pods -n $NS -o wide
```

**What you should see:**
```
devzero-zxporter-controller-manager-xxx   1/1   Running   0   30s
devzero-zxporter-controller-manager-yyy   1/1   Running   0   30s
zxporter-nodemon-aaa                      2/2   Running   0   30s   (one per node)
zxporter-nodemon-bbb                      2/2   Running   0   30s
zxporter-nodemon-ccc                      2/2   Running   0   30s
```

**Troubleshooting:**

| Problem | Command | Fix |
|---------|---------|-----|
| zxporter stuck at `0/1` | `kubectl logs deployment/devzero-zxporter-controller-manager -n $NS --tail=20` | Check for `invalid token` (bad token) or `connection refused` (wrong DAKR URL) |
| nodemon stuck at `0/2` | `kubectl describe pod -n $NS -l app.kubernetes.io/name=zxporter-nodemon \| tail -20` | Usually missing ConfigMaps — reapply manifest |
| nodemon `ImagePullBackOff` | `kubectl describe pod -n $NS -l app.kubernetes.io/name=zxporter-nodemon \| grep Image` | Wrong image tag — check the manifest |
| nodemon `CrashLoopBackOff` | `kubectl logs -n $NS -l app.kubernetes.io/name=zxporter-nodemon -c zxporter-nodemon --tail=20` | Check startup errors |

---

### Step 7: Verify data is flowing

**7a: Check zxporter is sending data:**
```bash
kubectl logs deployment/devzero-zxporter-controller-manager -n $NS --tail=30 \
  | grep -E "Successfully sent|container_resource|node_resource|error" \
  | tail -10
```

**You should see:**
```
Splitting resources into batches  resourceType: container_resource
Successfully sent batch  batchSize: 80
Splitting resources into batches  resourceType: node_resource
Successfully sent batch  batchSize: 4
```

**You should NOT see:**
```
"invalid token"              → wrong cluster token
"connection refused"         → wrong DAKR URL
"nodemon returned status 404" → old nodemon image (needs /v2/container/metrics)
"prometheus"                 → old binary still running
```

**7b: Check nodemon is serving metrics:**
```bash
NODEMON_IP=$(kubectl get pods -n $NS -l app.kubernetes.io/name=zxporter-nodemon \
  -o jsonpath='{.items[0].status.podIP}')

kubectl run verify --rm -i --restart=Never --image=curlimages/curl -n $NS \
  -- curl -s "http://$NODEMON_IP:6061/v2/container/metrics" | head -c 500
```

**You should see:** JSON with `cpu_usage_nanocores`, `memory_working_set_bytes`, `network_rx_bytes`, etc.

**7c: Check no Prometheus errors:**
```bash
kubectl logs deployment/devzero-zxporter-controller-manager -n $NS --tail=50 | grep -i "prometheus"
```
Should return nothing.

---

### Step 8: Verify on the DevZero dashboard

1. Open your DevZero dashboard
2. Find your cluster (same name as before)
3. Check:
   - **Cluster overview** — CPU/Memory utilization graphs show data (wait 2-3 minutes)
   - **Workloads** — CPU/Memory usage columns are non-zero
   - **Nodes** — Network and disk I/O visible

> **No data after 5 minutes?** Go back to Step 7 and check the logs.

---

### Step 9: Update DAKR operator (if namespace changed)

**Skip this step if:** your old zxporter was already in `devzero-system`.

**Do this step if:** your old zxporter was in `devzero-zxporter` (or any other namespace) and the new one is in `devzero-system`.

The DAKR operator connects to zxporter's MPA gRPC service using a URL that includes the namespace. If the namespace changed, the operator can't find zxporter.

**9a: Check the current DAKR operator setting:**
```bash
kubectl get deployment -n dakr-operator -l app.kubernetes.io/name=dakr-operator \
  -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' 2>/dev/null \
  | tr ',' '\n' | grep zxporter-addr
```

Example output:
```
"--zxporter-addr=devzero-zxporter-controller-manager-mpa.devzero-zxporter.svc.cluster.local:50051"
                                                         ^^^^^^^^^^^^^^^^
                                                         THIS IS THE OLD NAMESPACE
```

**9b: Verify where the MPA service actually is now:**
```bash
kubectl get service -A | grep mpa
```

Should show:
```
devzero-system   devzero-zxporter-controller-manager-mpa   ClusterIP   ...   50051/TCP
```

**9c: If the namespaces don't match, update the operator:**
```bash
helm upgrade dakr <your-dakr-operator-chart> \
  --namespace dakr-operator \
  --reuse-values \
  --set operator.zxporterAddr="devzero-zxporter-controller-manager-mpa.devzero-system.svc.cluster.local:50051"
```

**9d: Verify the operator connected:**
```bash
kubectl rollout status deployment -n dakr-operator -l app.kubernetes.io/name=dakr-operator --timeout=120s

# Check for MPA activity
kubectl logs deployment/$(kubectl get deployment -n dakr-operator -l app.kubernetes.io/name=dakr-operator \
  -o jsonpath='{.items[0].metadata.name}') -n dakr-operator --tail=20 \
  | grep -iE "mpa|rule.*eval|metrics.*batch"
```

**You should see:**
```
Initializing Rule Evaluator Controller (unified MPA)
Received metrics batch  item_count: 5
```

**If you see `Rule Evaluator Controller is disabled`:**
```bash
helm upgrade dakr <chart> --namespace dakr-operator --reuse-values \
  --set mpaController.enabled=true \
  --set operator.zxporterAddr="devzero-zxporter-controller-manager-mpa.devzero-system.svc.cluster.local:50051"
```

---

### Step 10: Clean up the temp file

```bash
rm -f /tmp/new-zxporter.yaml /tmp/zxporter-configmap.yaml /tmp/zxporter-secret.yaml /tmp/zxporter-credentials.yaml
echo "Migration complete!"
```

---

## Quick Reference: Resources Deleted by This Migration

### Prometheus Server
| Deployment | Service | ServiceAccount | ConfigMap | ClusterRole | ClusterRoleBinding |
|---|---|---|---|---|---|
| `prometheus-dz-prometheus-server` | `prometheus-dz-prometheus-server` | `prometheus-dz-prometheus-server` | `prometheus-dz-prometheus-server` | `prometheus-dz-prometheus-server` | `prometheus-dz-prometheus-server` |

### Kube-State-Metrics
| Deployment | Service | ServiceAccount | ClusterRole | ClusterRoleBinding |
|---|---|---|---|---|
| `prometheus-kube-state-metrics` | `prometheus-kube-state-metrics` | `prometheus-kube-state-metrics` | `prometheus-kube-state-metrics` | `prometheus-kube-state-metrics` |

### Node-Exporter
| DaemonSet | Service | ServiceAccount |
|---|---|---|
| `dz-prometheus-node-exporter` | `dz-prometheus-node-exporter` | `dz-prometheus-node-exporter` |

### Metrics-Server
| Deployment | Service | ServiceAccount | ClusterRoles | ClusterRoleBindings |
|---|---|---|---|---|
| `dz-metrics-server` | `dz-metrics-server` | `dz-metrics-server` | `system:dz-metrics-server`, `system:dz-metrics-server-aggregated-reader` | `dz-metrics-server:system:auth-delegator`, `system:dz-metrics-server` |

---

## FAQ

**Q: Will this affect my other Prometheus installation?**
No. Only deletes resources by exact name in the zxporter namespace.

**Q: How long is the data gap?**
2-5 minutes between Step 4 (delete) and Step 6 (new pods start sending).

**Q: Do I need to change my cluster token?**
No. Same token works. The token is tied to your cluster record, not the namespace.

**Q: What if I lost the token?**
Go to DevZero dashboard → Clusters → your cluster → generate a new token.

**Q: Can I still use `kubectl top` after migration?**
Yes, if your cluster has a managed metrics-server (EKS/GKE/AKS all do). If it breaks, the old zxporter's `dz-metrics-server` was serving the API — fix it:
```bash
kubectl patch apiservice v1beta1.metrics.k8s.io --type merge \
  -p '{"spec":{"service":{"name":"metrics-server","namespace":"kube-system","port":443}}}'
```

**Q: The cleanup Job creates extra resources (ServiceAccount, Role, etc). Is that a problem?**
No. The Job and its RBAC auto-delete after 5 minutes via `ttlSecondsAfterFinished`. In the manual migration path, the Job finds nothing to clean (you already did it in Step 4) and exits immediately.

**Q: What about `PROMETHEUS_URL` or `ENABLE_NODEMON_METRICS` in the old ConfigMap?**
Ignored. The new binary doesn't read them.

**Q: The namespace is stuck in Terminating forever. Help!**
See Step 4e — force-remove the finalizer, and fix the stale `v1beta1.metrics.k8s.io` APIService if present.

**Q: I see `Collector not available, skipping registration` for `container_resource` in the logs.**
Nodemon pods aren't discovered yet. Make sure nodemon is running in the **same namespace** as zxporter:
```bash
kubectl get pods -n $NS -l app.kubernetes.io/name=zxporter-nodemon
```
