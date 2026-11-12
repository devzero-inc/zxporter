# devzero-common

DevZero platform **Helm library chart** (D3 of the release-flow standardization —
see [`docs/plans/release-flow-standardization.md`](https://github.com/devzero-inc/services/blob/main/docs/plans/release-flow-standardization.md) §5).
It exports the shared named templates every DevZero subchart imports, so
cross-chart conventions (labels, image resolution, resources, telemetry, TLS,
secrets, service account) live in one place instead of ten independently-diverged
`_helpers.tpl` copies.

It is `type: library` — not installable on its own; it only ships `define`d
templates.

## Use it from a chart

Add the dependency in the consumer's `Chart.yaml`:

```yaml
dependencies:
  - name: devzero-common
    version: 0.1.0
    repository: file://../devzero-common   # once both live under charts/
```

Re-export the helpers as one-liners in the consumer's `templates/_helpers.tpl`
so local template names stay conventional:

```
{{- define "mychart.labels" -}}{{ include "devzero-common.labels" . }}{{- end -}}
{{- define "mychart.fullname" -}}{{ include "devzero-common.fullname" . }}{{- end -}}
```

## Exported templates

| Template | Emits | Typical usage |
|---|---|---|
| `devzero-common.name` / `.fullname` / `.chart` | name strings | metadata.name, chart label |
| `devzero-common.labels` | full label set (`app.kubernetes.io/*`, `part-of: dz-platform`, `devzero.io/component`, + `global.commonLabels`). `commonLabels` may **add** keys but never override a standard one — the standard keys carry the selector labels, so letting them be reassigned could desync a workload's labels from its immutable selector. | `labels: {{- include ... \| nindent 4 }}` |
| `devzero-common.selectorLabels` | immutable subset (name + instance) | selector `matchLabels` |
| `devzero-common.image` | `<registry>/<repository>:<tag>` (registry ← `global.imageRegistry`→`image.registry`; tag ← `image.tag`→`.Chart.AppVersion`) | `image: {{ include ... }}` |
| `devzero-common.imageRef` | a full image reference with its registry host replaced by `global.imageRegistry`, keeping the path, tag and digest (`registry.k8s.io/pause:3.9` → `<global>/pause:3.9`, `alpine:3.19` → `<global>/library/alpine:3.19`); the input unchanged when the global is empty. For charts that build references from many keys rather than one `image` object. Tests: `tools/chartlibtest` | `image: {{ include ... (dict "ref" $ref "global" .Values.global) \| quote }}` |
| `devzero-common.imagePullSecrets` | merged `global.imagePullSecrets` + `image.pullSecrets` block, deduped by name (accepts `["name"]` and `[{name: ...}]`, and dedupes across the two shapes) | `{{- include ... \| nindent 6 }}` |
| `devzero-common.clusterName` / `.dakrUrl` / `.k8sProvider` | `global.clusterName` / `global.dakrUrl` / `global.k8sProvider` when set, else the chart's own value (the umbrella sets each once for every component; a standalone install keeps its own key) | `{{ include "devzero-common.dakrUrl" (list . .Values.dakrUrl) \| quote }}` |
| `devzero-common.resources` | `.Values.resources`, else a conservative default | `resources: {{- include ... \| nindent 12 }}` |
| `devzero-common.telemetry` | OTLP env vars from `global.telemetry.otlp` (empty when disabled) | container `env:` |
| `devzero-common.serviceAccount` / `.serviceAccount.name` | ServiceAccount object / resolved name | `templates/serviceaccount.yaml`, `serviceAccountName:` |
| `devzero-common.tls.certificate` | cert-manager `Certificate` when `global.tls.certManagerIssuerRef` is set | `templates/certificate.yaml` |
| `devzero-common.secretName` / `.createSecret` | resolved secret name / whether to create inline | `secretKeyRef.name`, `{{- if eq (include "devzero-common.createSecret" .) "true" }}` |

`global.*` keys are the umbrella pass-through surface (D5): the platform umbrella
fans `global.imageRegistry`, `global.imagePullSecrets`, `global.telemetry`,
`global.tls`, `global.commonLabels`, and the cluster identity
(`global.clusterName`, `global.dakrUrl`, `global.k8sProvider`) into every
subchart automatically. A component honours a key only once its chart calls
the matching helper.

## Values contract (enforced by `just chart-lint`)

Consumer charts must follow the shared schema: `image` is always an **object**
(`registry`/`repository`/`tag`/`pullPolicy`/`pullSecrets`), never a bare string;
`serviceAccount.{create,name,annotations,automount}` (`automount: false` is honoured — it is not treated as unset); feature flags under
`features.<name>.enabled`. See §5 of the plan for the full schema.

## Lint

```sh
just chart-lint        # helm lint + layout conventions on every chart under charts/
```
