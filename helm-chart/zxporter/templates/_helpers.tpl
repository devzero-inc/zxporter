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
Validate required configuration
*/}}
{{- define "zxporter.validateConfig" -}}
  {{- if and (empty .Values.zxporter.clusterToken) (empty .Values.zxporter.patToken) -}}
    {{- fail "ERROR: Either zxporter.clusterToken or zxporter.patToken must be provided. Please set one of these values in your values file." -}}
  {{- end -}}
  
  {{- if empty .Values.zxporter.kubeContextName -}}
    {{- fail "ERROR: zxporter.kubeContextName is required. Please provide a unique identifier for your cluster." -}}
  {{- end -}}
  
  {{- if empty .Values.zxporter.k8sProvider -}}
    {{- fail "ERROR: zxporter.k8sProvider is required. Please set it to one of: aws, gcp, azure, oci, other" -}}
  {{- end -}}
  
  {{- $validProviders := list "aws" "gcp" "azure" "oci" "other" -}}
  {{- if not (has .Values.zxporter.k8sProvider $validProviders) -}}
    {{- fail (printf "ERROR: zxporter.k8sProvider must be one of: %s. Got: %s" (join ", " $validProviders) .Values.zxporter.k8sProvider) -}}
  {{- end -}}
{{- end }}