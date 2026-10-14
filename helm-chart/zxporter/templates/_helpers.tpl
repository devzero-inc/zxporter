{{/*
Expand the name of the chart.
*/}}
{{- define "zxporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "zxporter.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "zxporter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "zxporter.labels" -}}
helm.sh/chart: {{ include "zxporter.chart" . }}
{{ include "zxporter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with (include "zxporter.commonLabels" .) }}
{{ . }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "zxporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zxporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Render a pod's topologySpreadConstraints list. Called with
(dict "constraints" <list> "selector" <map>): an entry with no labelSelector
gets one matching the pod's own labels, so a user can add the spread rule an
admission policy requires without knowing the release-derived labels.
*/}}
{{- define "zxporter.topologySpreadConstraints" -}}
{{- $out := list }}
{{- range .constraints }}
{{- $c := deepCopy . }}
{{- if not (hasKey $c "labelSelector") }}
{{- $_ := set $c "labelSelector" (dict "matchLabels" $.selector) }}
{{- end }}
{{- $out = append $out $c }}
{{- end }}
{{- toYaml $out }}
{{- end }}

{{/*
The controller image: image.repository:image.tag, with its registry host
swapped for global.imageRegistry when the dz-platform umbrella sets it. With
the global unset this is exactly the reference the chart always rendered.
*/}}
{{- define "zxporter.image" -}}
{{- /* Each key renders as the chart always printed it: null as empty, any
     other value (a tag of 0 or false included) as its string. `default`
     would turn 0 and false into empty. */ -}}
{{- $repository := .Values.image.repository -}}
{{- $tag := .Values.image.tag -}}
{{- $ref := printf "%s:%s" (ternary "" (toString $repository) (kindIs "invalid" $repository)) (ternary "" (toString $tag) (kindIs "invalid" $tag)) -}}
{{- include "devzero-common.imageRef" (dict "ref" $ref "global" .Values.global) -}}
{{- end }}

{{/*
The pod's imagePullSecrets block: global.imagePullSecrets merged with the
chart's own imagePullSecrets, names or {name: ...} objects, deduplicated.
Empty, and so rendering nothing, when neither is set.
*/}}
{{- define "zxporter.imagePullSecrets" -}}
{{- include "devzero-common.imagePullSecrets" (dict "Values" (dict "global" .Values.global "image" (dict "pullSecrets" .Values.imagePullSecrets))) -}}
{{- end }}

{{/*
Validate required configuration
*/}}
{{- define "zxporter.validateConfig" -}}
  {{- if and (empty .Values.zxporter.clusterToken) (empty .Values.zxporter.patToken) (empty (dig "existingSecret" "name" "" .Values.zxporter)) -}}
    {{- fail "ERROR: A token must be provided. Set one of zxporter.clusterToken, zxporter.patToken, or zxporter.existingSecret.name (a pre-existing Secret holding the token)." -}}
  {{- end -}}

  {{- $existingName := dig "existingSecret" "name" "" .Values.zxporter -}}
  {{- if and $existingName (eq $existingName .Values.zxporter.tokenSecretName) -}}
    {{- fail (printf "ERROR: zxporter.existingSecret.name (%q) must not equal zxporter.tokenSecretName. The chart creates and writes to that runtime Secret itself, so reusing the name would collide on install and let the controller overwrite your managed Secret." .Values.zxporter.tokenSecretName) -}}
  {{- end -}}

  {{- if empty (include "devzero-common.clusterName" (list . .Values.zxporter.kubeContextName)) -}}
    {{- fail "ERROR: zxporter.kubeContextName (or global.clusterName) is required. Please provide a unique identifier for your cluster." -}}
  {{- end -}}
  
  {{- if empty .Values.global.k8sProvider -}}
    {{- fail "ERROR: global.k8sProvider is required. Please set it to one of: aws, gcp, azure, oci, other" -}}
  {{- end -}}

  {{- $validProviders := list "aws" "gcp" "azure" "oci" "other" -}}
  {{- if not (has .Values.global.k8sProvider $validProviders) -}}
    {{- fail (printf "ERROR: global.k8sProvider must be one of: %s. Got: %s" (join ", " $validProviders) .Values.global.k8sProvider) -}}
  {{- end -}}

  {{- $gpuRuntimeMode := default "auto" (dig "gpuRuntime" "mode" "" .Values.global) -}}
  {{- $validGpuRuntimeModes := list "auto" "default" "explicit" -}}
  {{- if not (has $gpuRuntimeMode $validGpuRuntimeModes) -}}
    {{- fail (printf "ERROR: global.gpuRuntime.mode must be one of: %s. Got: %s" (join ", " $validGpuRuntimeModes) $gpuRuntimeMode) -}}
  {{- end -}}

  {{- $gpuRuntimeClassName := dig "gpuRuntime" "runtimeClassName" "" .Values.global -}}
  {{- if and (ne $gpuRuntimeMode "default") (empty $gpuRuntimeClassName) -}}
    {{- fail (printf "ERROR: global.gpuRuntime.runtimeClassName is required when global.gpuRuntime.mode is %s." $gpuRuntimeMode) -}}
  {{- end -}}
{{- end }}

{{/*
Site-wide labels for object metadata.

Clusters running an admission policy engine (OPA Gatekeeper, Kyverno) often
require site-specific metadata on every object before they will admit it. Every
key the chart owns is dropped from the user-supplied values, so a collision can
neither rewrite chart state nor emit a duplicate YAML key.

`global.commonLabels` is the knob that reaches subcharts; a chart-local
`commonLabels` applies only to the chart it is set on and wins on a collision.

Every object also carries devzero-common's platform labels,
app.kubernetes.io/part-of and devzero.io/component, so all DevZero charts can
be selected the same way. A user who already set either key through
commonLabels keeps their value. Pod templates do not get them: those labels
come from podExtraLabels, and selectors never change.
*/}}
{{- define "zxporter.commonLabels" -}}
{{- $extra := deepCopy (.Values.commonLabels | default dict) -}}
{{- if .Values.global }}{{- with .Values.global.commonLabels }}{{- $extra = merge $extra (deepCopy .) }}{{- end }}{{- end }}
{{- /* Completeness of this list is enforced by the "no chart-emitted key can
       produce a duplicate" assertion in helm-chart/internal/charttest, which
       derives the real set from a render rather than restating it here. */}}
{{- range $key := list "helm.sh/chart" "app.kubernetes.io/name" "app.kubernetes.io/instance" "app.kubernetes.io/version" "app.kubernetes.io/managed-by" "app.kubernetes.io/component" "app" "control-plane" }}
{{- $_ := unset $extra $key }}
{{- end }}
{{- toYaml (merge $extra (include "zxporter.platformLabels" . | fromYaml)) }}
{{- end }}

{{/*
devzero-common's platform labels: part-of and component only. The chart keeps
its own name, instance, version, managed-by and helm.sh/chart labels, and its
own selectors, so nothing that selects on them changes.
*/}}
{{- define "zxporter.platformLabels" -}}
{{- toYaml (pick (include "devzero-common.labels" . | fromYaml) "app.kubernetes.io/part-of" "devzero.io/component") }}
{{- end }}

{{/*
Site-wide annotations for object metadata.
*/}}
{{- define "zxporter.commonAnnotations" -}}
{{- $extra := deepCopy (.Values.commonAnnotations | default dict) -}}
{{- if .Values.global }}{{- with .Values.global.commonAnnotations }}{{- $extra = merge $extra (deepCopy .) }}{{- end }}{{- end }}
{{- range $key := list "helm.sh/hook" "helm.sh/hook-weight" "helm.sh/hook-delete-policy" "kubectl.kubernetes.io/default-container" "devzero.io/gpu-runtime-mode" "devzero.io/gpu-runtime-class" "ignore-check.kube-linter.io/privileged-container" "ignore-check.kube-linter.io/run-as-non-root" "ignore-check.kube-linter.io/privilege-escalation-container" "ignore-check.kube-linter.io/no-read-only-root-fs" }}
{{- $_ := unset $extra $key }}
{{- end }}
{{- with $extra }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/*
Pod-template labels: commonLabels plus podLabels, podLabels winning.

Gatekeeper's expand-workloads (and Kyverno's equivalent) evaluates constraints
against the Pod a workload would create, so site labels have to reach the pod
template and not just the owning object. Keys the pod templates set themselves
are dropped: several of them are selector labels, which are immutable after
creation.
*/}}
{{- define "zxporter.podExtraLabels" -}}
{{- $extra := deepCopy (.Values.podLabels | default dict) -}}
{{- if .Values.global }}{{- with .Values.global.podLabels }}{{- $extra = merge $extra (deepCopy .) }}{{- end }}{{- end }}
{{- $extra = merge $extra (deepCopy (.Values.commonLabels | default dict)) -}}
{{- if .Values.global }}{{- with .Values.global.commonLabels }}{{- $extra = merge $extra (deepCopy .) }}{{- end }}{{- end }}
{{- range $key := list "helm.sh/chart" "app.kubernetes.io/name" "app.kubernetes.io/instance" "app.kubernetes.io/version" "app.kubernetes.io/managed-by" "app.kubernetes.io/component" "app" "control-plane" }}
{{- $_ := unset $extra $key }}
{{- end }}
{{- with $extra }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/*
Pod-template annotations: commonAnnotations plus podAnnotations.
*/}}
{{- define "zxporter.podExtraAnnotations" -}}
{{- $extra := deepCopy (.Values.podAnnotations | default dict) -}}
{{- if .Values.global }}{{- with .Values.global.podAnnotations }}{{- $extra = merge $extra (deepCopy .) }}{{- end }}{{- end }}
{{- $extra = merge $extra (deepCopy (.Values.commonAnnotations | default dict)) -}}
{{- if .Values.global }}{{- with .Values.global.commonAnnotations }}{{- $extra = merge $extra (deepCopy .) }}{{- end }}{{- end }}
{{- range $key := list "helm.sh/hook" "helm.sh/hook-weight" "helm.sh/hook-delete-policy" "kubectl.kubernetes.io/default-container" "devzero.io/gpu-runtime-mode" "devzero.io/gpu-runtime-class" "ignore-check.kube-linter.io/privileged-container" "ignore-check.kube-linter.io/run-as-non-root" "ignore-check.kube-linter.io/privilege-escalation-container" "ignore-check.kube-linter.io/no-read-only-root-fs" }}
{{- $_ := unset $extra $key }}
{{- end }}
{{- with $extra }}
{{- toYaml . }}
{{- end }}
{{- end }}
