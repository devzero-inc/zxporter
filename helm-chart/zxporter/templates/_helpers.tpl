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
{{- end }}

{{/*
Selector labels
*/}}
{{- define "zxporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zxporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Effective Kubernetes provider.
Single source of truth is global.k8sProvider, which is also shared with the
zxporter-nodemon subchart. Falls back to the legacy zxporter.k8sProvider for
backward compatibility with existing values files.
*/}}
{{- define "zxporter.k8sProvider" -}}
{{- $global := .Values.global | default dict -}}
{{- $global.k8sProvider | default .Values.zxporter.k8sProvider | default "" -}}
{{- end }}

{{/*
Validate required configuration
*/}}
{{- define "zxporter.validateConfig" -}}
  {{- if and (empty .Values.zxporter.clusterToken) (empty .Values.zxporter.patToken) -}}
    {{- fail "ERROR: Either zxporter.clusterToken or zxporter.patToken must be provided. Please set one of these values in your values file." -}}
  {{- end -}}

  {{- if empty .Values.zxporter.kubeContextName -}}
    {{- fail "ERROR: zxporter.kubeContextName is required. Please provide a unique identifier for your cluster." -}}
  {{- end -}}

  {{- $provider := include "zxporter.k8sProvider" . -}}
  {{- if empty $provider -}}
    {{- fail "ERROR: global.k8sProvider is required. Please set it to one of: aws, gcp, azure, oci, other" -}}
  {{- end -}}

  {{- $validProviders := list "aws" "gcp" "azure" "oci" "other" -}}
  {{- if not (has $provider $validProviders) -}}
    {{- fail (printf "ERROR: global.k8sProvider must be one of: %s. Got: %s" (join ", " $validProviders) $provider) -}}
  {{- end -}}
{{- end }}