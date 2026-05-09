# Cluster Identity: Stable Cluster Identification Across Reinstalls

## Overview

By default, every time you install or reinstall ZXPorter, it calls `CreateClusterToken` on the DevZero platform — which creates a **brand new cluster entry**. This means if you uninstall and reinstall ZXPorter on the same physical cluster, the DevZero dashboard shows it as a different cluster.

**Cluster Identity** solves this. By giving your cluster a stable identifier, ZXPorter calls `ReattachCluster` instead, which **finds or creates** a cluster by that identifier. Reinstalling ZXPorter always connects back to the same cluster entry in the DevZero platform.

---

## End-User Guide

### When do you need this?

- You want your cluster to always appear as the same entry in the DevZero dashboard
- You manage multiple clusters and need stable, human-readable names

### Option A: Let Helm manage the Secret (simplest)

Set `clusterIdentifier` in your `values.yaml`. Helm will auto-create a Kubernetes Secret that persists the identifier.

```yaml
zxporter:
  patToken: "dzu-your-pat-token"
  clusterIdentifier: "prod-us-east"   # stable DNS-label style name
  clusterIdentitySecretName: "devzero-zxporter-cluster-identity"  # default
```

Run `helm upgrade --install`:

```bash
helm upgrade --install zxporter ./helm-chart/zxporter \
  -n devzero-zxporter --create-namespace \
  -f values.yaml
```

Helm creates the Secret automatically with `helm.sh/resource-policy: keep`, meaning the Secret **survives `helm uninstall`**. On the next install, ZXPorter reads the identifier from the Secret and reattaches to the same cluster.

> **Note:** The Secret wins over `clusterIdentifier` in `values.yaml`. Once the Secret exists, you can clear `clusterIdentifier` from your values file — the Secret is the source of truth.

---

### Option B: Bring your own Secret (production/GitOps)

Create the Secret manually before installing ZXPorter:

```bash
kubectl create secret generic devzero-zxporter-cluster-identity \
  -n devzero-zxporter \
  --from-literal=CLUSTER_IDENTIFIER=prod-us-east
```

Leave `clusterIdentifier` empty in `values.yaml` and just point to the Secret:

```yaml
zxporter:
  patToken: "dzu-your-pat-token"
  clusterIdentifier: ""
  clusterIdentitySecretName: "devzero-zxporter-cluster-identity"
```

This is the recommended approach for GitOps workflows where you don't want the identifier hardcoded in values files.

---

### What happens on each install?

| Scenario | Behavior |
|---|---|
| Secret exists with `CLUSTER_IDENTIFIER` | Reads identifier from Secret → calls `ReattachCluster` → connects to existing cluster |
| No Secret, `clusterIdentifier` set in values | Uses values.yaml identifier → calls `ReattachCluster` → Helm auto-creates the Secret |
| No Secret, no `clusterIdentifier` | Calls `CreateClusterToken` → creates a new cluster entry |
| Secret exists but `CLUSTER_IDENTIFIER` key is missing/empty | Falls back to `clusterIdentifier` in values.yaml; if also empty → creates new cluster |

---

### What if someone deletes the Secret?

If the Secret is deleted but a `CLUSTER_IDENTIFIER` is set in `values.yaml`, ZXPorter falls back to that value and still calls `ReattachCluster`. The next `helm upgrade` recreates the Secret.

If both are gone, ZXPorter creates a new cluster entry on the DevZero platform. You would need to manually reconcile or set `clusterIdentifier` again.

---

### What if someone deletes the Secret and forgets the identifier?

Without the identifier, ZXPorter creates a new cluster. To recover:
1. Find the cluster identifier from the DevZero dashboard or your previous values file
2. Recreate the Secret manually (Option B above)
3. Run `helm upgrade --install` to reconnect

---

## Developer Guide

### Architecture

The cluster identity flow lives in `internal/controller/custom.go`, inside `initializeTelemetryComponents`.

#### Priority chain (highest to lowest)

```
1. Kubernetes Secret (CLUSTER_IDENTITY_SECRET_NAME)
   └─ Key: CLUSTER_IDENTIFIER
2. values.yaml / ConfigMap env var
   └─ Key: CLUSTER_IDENTIFIER
3. No identifier → CreateClusterToken (new cluster)
```

#### How the identifier is resolved

```go
// 1. Read secret name from ConfigMap (mounted as file at /etc/zxporter/config/)
clusterIdentitySecretName := os.Getenv("CLUSTER_IDENTITY_SECRET_NAME")
if clusterIdentitySecretName == "" {
    // fallback: read from mounted config file
}

// 2. If secret name is set, read CLUSTER_IDENTIFIER from that secret
if clusterIdentitySecretName != "" {
    if identifier := c.readClusterIdentifierFromSecret(ctx, clusterIdentitySecretName); identifier != "" {
        envSpec.Policies.ClusterIdentifier = identifier  // secret wins
    }
}

// 3. If no stored token, decide which RPC to call
if envSpec.Policies.ClusterIdentifier != "" {
    token, clusterId, err = tempClient.ReattachCluster(...)
} else {
    token, clusterId, err = tempClient.ExchangePATForClusterToken(...)
}
```

#### `readClusterIdentifierFromSecret` (`custom.go`)

- Read-only. The operator **never writes** to the identity Secret.
- Reads `CLUSTER_IDENTIFIER` key from the named Secret in the operator's namespace.
- Returns empty string on any error (not found, key missing, permission denied) — never blocks startup.
- Logs a hint if the key is missing, telling the user to set `clusterIdentifier` in values.yaml.

#### `ReattachCluster` RPC (`internal/transport/dakr_client.go`)

Calls the `ReattachCluster` endpoint on the DevZero platform with:
- `pat_token` — the user's Personal Access Token
- `cluster_identifier` — the stable identifier
- `cluster_name` — from `kubeContextName` in values.yaml
- `k8s_provider` — from `k8sProvider` in values.yaml

The platform does a find-or-create: if a cluster with that identifier already exists under the PAT's account, it returns the existing cluster token. Otherwise it creates a new cluster with that identifier.

#### Token persistence

After a successful exchange (via either `ReattachCluster` or `ExchangePATForClusterToken`), the token **and the identifier** are persisted via `persistClusterToken`. This writes to either:
- **ConfigMap** (`devzero-zxporter-env-config`) — default when `useSecretForToken: false`
- **Kubernetes Secret** (`devzero-zxporter-token`) — when `useSecretForToken: true`

On the **next startup**, `tryRecoverStoredClusterToken` finds the stored token and skips the PAT exchange entirely. This means the PAT is only used once per "new cluster" event.

---

### Helm templates involved

| File | Purpose |
|---|---|
| `templates/secret-cluster-identity.yaml` | Auto-creates the identity Secret when `clusterIdentifier` is set. Annotated with `helm.sh/resource-policy: keep` so it survives `helm uninstall`. |
| `templates/configmap.yaml` | Includes `CLUSTER_IDENTITY_SECRET_NAME` so the operator knows which Secret to read. Only emits `CLUSTER_TOKEN` when non-empty (avoids Helm field ownership conflicts on upgrade). |
| `templates/zxporter-rbac.yaml` | Grants `get` on the identity Secret unconditionally (not gated by `useSecretForToken`), so the operator can always read the identifier. |

---

### RBAC

The operator needs read access to the cluster identity Secret. This is granted in the `devzero-zxporter-leader-election-role` (namespace-scoped Role):

```yaml
- apiGroups: [""]
  resourceNames: ["devzero-zxporter-cluster-identity"]
  resources: ["secrets"]
  verbs: ["get"]
```

This is **not** gated by `useSecretForToken` — the identity Secret is separate from the token storage Secret.

---

### Key design decisions

**Why a Secret instead of just values.yaml?**
- values.yaml changes on upgrade. If you accidentally clear `clusterIdentifier`, you'd create a new cluster.
- Secrets annotated with `helm.sh/resource-policy: keep` survive `helm uninstall`, making them the durable source of truth.

**Why is the operator read-only on the identity Secret?**
- The identity Secret is user-managed. The operator reading it doesn't need write access, which follows least-privilege.
- The token storage Secret (`devzero-zxporter-token`) is separately managed with read/write access.

**Why ConfigMap-as-volume instead of env vars?**
- ZXPorter uses `configmap` mounted as a volume at `/etc/zxporter/config/`. Files in that directory reflect ConfigMap changes without pod restarts.
- `CLUSTER_IDENTITY_SECRET_NAME` is passed via this mechanism.
