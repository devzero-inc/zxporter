# Use an existing Secret for the token (SOPS / GitOps)

Instead of putting `clusterToken` / `patToken` in your values, point the chart
at a Secret you manage. You encrypt only that Secret with
[SOPS](https://github.com/getsops/sops); your values file stays token-free.

## The one flag that matters

Take the helm command from the dashboard's **Connect cluster** button and add:

```
--set zxporter.existingSecret.name=<your-secret-name>
```

That's it — the chart then reads the token from your Secret instead of creating
its own, and no token ever appears in the helm command or values.

## Steps (SOPS + age)

```bash
export NS=devzero-system

# 1. Create the SOPS-encrypted Secret (encrypt only the data; commit the .enc.yaml).
age-keygen -o key.txt                      # prints: Public key: age1xxxx...
cat > zxporter-token.secret.yaml <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: zxporter-token
type: Opaque
stringData:
  PAT_TOKEN: "dzp_your_pat"                 # or CLUSTER_TOKEN: "..."
EOF
sops --encrypt --age age1xxxx... --encrypted-regex '^(data|stringData)$' \
  zxporter-token.secret.yaml > zxporter-token.secret.enc.yaml
rm zxporter-token.secret.yaml

# 2. Apply it into the namespace (server-side keeps plaintext out of annotations).
kubectl create namespace $NS --dry-run=client -o yaml | kubectl apply -f -
SOPS_AGE_KEY_FILE=$PWD/key.txt sops --decrypt zxporter-token.secret.enc.yaml \
  | kubectl apply --server-side -n $NS -f -

# 3. Run the dashboard's Connect-cluster helm command with the extra flag:
#      --set zxporter.existingSecret.name=zxporter-token
```

**GitOps:** commit `zxporter-token.secret.enc.yaml` and let Argo CD (KSOPS /
`argocd-vault-plugin`) or Flux (`decryption.provider: sops`) decrypt it at sync
time; set `zxporter.existingSecret.name` in the HelmRelease/Application values.

## Values

```yaml
zxporter:
  existingSecret:
    name: ""                       # your Secret; empty = use clusterToken/patToken
    clusterTokenKey: "CLUSTER_TOKEN"
    patTokenKey: "PAT_TOKEN"
```

Provide one token — `CLUSTER_TOKEN` or `PAT_TOKEN`. Only used when
`useSecretForToken: true` (the default).

## Gotchas

- **Secret must exist in the release namespace before install** (`secretKeyRef`
  is namespace-local).
- **Name must differ from `tokenSecretName`** (default `devzero-zxporter-token`) —
  the chart manages that one itself. The chart fails fast if they match.
- **Wrong/missing key = no auth, no crash** (keys are `optional`). Check pod logs
  for `no URL or token was configured`.
