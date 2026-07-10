# Use an existing Kubernetes Secret for the token

By default the chart creates the token Secret from `zxporter.clusterToken` /
`zxporter.patToken`. Instead, you can create the Secret yourself and point the
chart at it — the token then never passes through the Helm command or values file.
Good for GitOps, and for encrypting the Secret with
[SOPS](https://github.com/getsops/sops) (see [Option B](#option-b-manage-with-sops)).

## What the chart needs

1. **The Secret exists** in the release namespace before install (`secretKeyRef` is
   namespace-local).
2. **It holds the token** under `PAT_TOKEN` and/or `CLUSTER_TOKEN` (one is enough).
   Override the key names with `existingSecret.clusterTokenKey` / `patTokenKey`.
3. **You pass its name** — append to the dashboard's **Connect cluster** Helm command:
```
--set zxporter.existingSecret.name=<your-secret-name>
```

Setting `existingSecret.name` satisfies the chart's "token required" check, so you
don't also need `clusterToken`/`patToken`.

## Option A: plain Secret

```bash
export NS=devzero-system
kubectl create namespace $NS --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic zxporter-token -n $NS \
  --from-literal=PAT_TOKEN='dzp_your_pat'
```

Then run the Connect-cluster command with
`--set zxporter.existingSecret.name=zxporter-token`.

## Option B: manage with SOPS

```bash
export NS=devzero-system

# Reuse your Age key, or generate one if you don't have it (a shared key lets
# teammates and your GitOps controller decrypt the same Secret).
age-keygen -o key.txt                          # prints: Public key: age1xxxx...

cat > zxporter-token.secret.yaml <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: zxporter-token
type: Opaque
stringData:
  PAT_TOKEN: "dzp_your_pat"
EOF
sops --encrypt --age age1xxxx... --encrypted-regex '^(data|stringData)$' \
  zxporter-token.secret.yaml > zxporter-token.secret.enc.yaml
rm zxporter-token.secret.yaml                   # keep only the encrypted file

# Apply it (--server-side keeps the plaintext out of the last-applied annotation).
kubectl create namespace $NS --dry-run=client -o yaml | kubectl apply -f -
SOPS_AGE_KEY_FILE=$PWD/key.txt sops --decrypt zxporter-token.secret.enc.yaml \
  | kubectl apply --server-side -n $NS -f -
```

Then run the Connect-cluster command with
`--set zxporter.existingSecret.name=zxporter-token`.

**GitOps:** commit `zxporter-token.secret.enc.yaml` and let Argo CD (KSOPS) or
Flux (`decryption.provider: sops`) decrypt it at sync time; set
`zxporter.existingSecret.name` in the HelmRelease/Application values.

## Via values.yaml

```yaml
zxporter:
  existingSecret:
    name: zxporter-token           # empty = chart creates its own Secret
    clusterTokenKey: "CLUSTER_TOKEN"   # override only if your keys differ
    patTokenKey: "PAT_TOKEN"
```

`useSecretForToken` defaults to `true`. (Turning it off puts tokens in a
ConfigMap and ignores `existingSecret`.)

## Gotchas

- **Name must differ from `tokenSecretName`** (default `devzero-zxporter-token`) —
  the chart owns that runtime Secret, so a matching name fails the install.
- **A wrong/missing key won't crash the pod** (keys are `optional`) — it just can't
  authenticate. Check logs for `no URL or token was configured`.
