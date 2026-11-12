{{/*
Name / fullname / chart helpers. Consumers re-export these from their own
templates/_helpers.tpl as one-liners, e.g.
  {{- define "trezr.fullname" -}}{{ include "devzero-common.fullname" . }}{{- end -}}
*/}}

{{- define "devzero-common.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "devzero-common.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "devzero-common.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Full label set. app.kubernetes.io/version falls back to the chart version when
a chart ships no appVersion (e.g. placeholder charts). part-of is fixed to the
platform; component is the chart name. global.commonLabels (umbrella pass-through)
can ADD keys but never override a standard one — see below.
*/}}
{{- define "devzero-common.labels" -}}
{{- $standard := dict
      "helm.sh/chart" (include "devzero-common.chart" .)
      "app.kubernetes.io/version" (.Chart.AppVersion | default .Chart.Version)
      "app.kubernetes.io/managed-by" .Release.Service
      "app.kubernetes.io/part-of" "dz-platform"
      "devzero.io/component" .Chart.Name
-}}
{{- /* selectorLabels stays the single source of truth for name/instance. */ -}}
{{- $standard = merge $standard (include "devzero-common.selectorLabels" . | fromYaml) -}}
{{- /* One merged dict emitted once, rather than appending commonLabels as extra
     lines. Appending would emit DUPLICATE YAML keys on collision, where the last
     value silently wins — and since this set carries the selector labels, a
     stray app.kubernetes.io/name in commonLabels could desync a workload's
     metadata labels from its immutable selector. sprig's `merge` gives
     precedence to the destination, so the standard keys win. */ -}}
{{- toYaml (merge $standard ((.Values.global | default dict).commonLabels | default dict)) | trim -}}
{{- end -}}

{{/*
Selector labels — the immutable subset. Never add to this: Deployment/StatefulSet
selectors are immutable, so anything here is locked for the life of the workload.
*/}}
{{- define "devzero-common.selectorLabels" -}}
app.kubernetes.io/name: {{ include "devzero-common.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
