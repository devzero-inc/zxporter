{{/*
Expand the name of the chart.
*/}}
{{- define "zxporter-netmon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The netmon image: image.repository:image.tag (the appVersion when the tag is
empty), with its registry host swapped for global.imageRegistry when the
dz-platform umbrella sets it. The default repository has no host, so it is a
Docker Hub path: devzeroinc/zxporter-netmon becomes
<global.imageRegistry>/devzeroinc/zxporter-netmon. With the global unset this
is exactly the reference the chart always rendered.
*/}}
{{- define "zxporter-netmon.image" -}}
{{- /* Each key renders as the chart always printed it: the repository
     verbatim (null as empty, 0 or false as its string), the tag falling back
     to the appVersion when empty (0 and false included, as before). */ -}}
{{- $repository := .Values.image.repository -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- $ref := printf "%s:%s" (ternary "" (toString $repository) (kindIs "invalid" $repository)) (ternary "" (toString $tag) (kindIs "invalid" $tag)) -}}
{{- include "devzero-common.imageRef" (dict "ref" $ref "global" .Values.global) -}}
{{- end }}

{{/*
The pod's imagePullSecrets block: global.imagePullSecrets merged with the
chart's own imagePullSecrets, names or {name: ...} objects, deduplicated.
Empty, and so rendering nothing, when neither is set.
*/}}
{{- define "zxporter-netmon.imagePullSecrets" -}}
{{- include "devzero-common.imagePullSecrets" (dict "Values" (dict "global" .Values.global "image" (dict "pullSecrets" .Values.imagePullSecrets))) -}}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "zxporter-netmon.fullname" -}}
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
{{- define "zxporter-netmon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "zxporter-netmon.labels" -}}
helm.sh/chart: {{ include "zxporter-netmon.chart" . }}
{{ include "zxporter-netmon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with (include "zxporter-netmon.commonLabels" .) }}
{{ . }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "zxporter-netmon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zxporter-netmon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app: zxporter-netmon
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "zxporter-netmon.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "zxporter-netmon.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
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
{{- define "zxporter-netmon.commonLabels" -}}
{{- $extra := deepCopy (.Values.commonLabels | default dict) -}}
{{- if .Values.global }}{{- with .Values.global.commonLabels }}{{- $extra = merge $extra (deepCopy .) }}{{- end }}{{- end }}
{{- /* Completeness of this list is enforced by the "no chart-emitted key can
       produce a duplicate" assertion in helm-chart/internal/charttest, which
       derives the real set from a render rather than restating it here. */}}
{{- range $key := list "helm.sh/chart" "app.kubernetes.io/name" "app.kubernetes.io/instance" "app.kubernetes.io/version" "app.kubernetes.io/managed-by" "app.kubernetes.io/component" "app" "control-plane" }}
{{- $_ := unset $extra $key }}
{{- end }}
{{- toYaml (merge $extra (include "zxporter-netmon.platformLabels" . | fromYaml)) }}
{{- end }}

{{/*
devzero-common's platform labels: part-of and component only. The chart keeps
its own name, instance, version, managed-by and helm.sh/chart labels, and its
own selectors, so nothing that selects on them changes.
*/}}
{{- define "zxporter-netmon.platformLabels" -}}
{{- toYaml (pick (include "devzero-common.labels" . | fromYaml) "app.kubernetes.io/part-of" "devzero.io/component") }}
{{- end }}

{{/*
Site-wide annotations for object metadata.
*/}}
{{- define "zxporter-netmon.commonAnnotations" -}}
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
{{- define "zxporter-netmon.podExtraLabels" -}}
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
{{- define "zxporter-netmon.podExtraAnnotations" -}}
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
