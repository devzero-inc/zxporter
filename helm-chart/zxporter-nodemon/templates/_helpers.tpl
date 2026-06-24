{{/*
Expand the name of the chart.
*/}}
{{- define "zxporter-nodemon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "zxporter-nodemon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "zxporter-nodemon.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "zxporter-nodemon.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "zxporter-nodemon.fullname" -}}
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
Selector labels
*/}}
{{- define "zxporter-nodemon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zxporter-nodemon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
GPU metrics exporter labels
*/}}
{{- define "zxporter-nodemon.labels" -}}
helm.sh/chart: {{ include "zxporter-nodemon.chart" . }}
{{ include "zxporter-nodemon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Create the name of the dcgm-exporter config map
*/}}
{{- define "dcgm-exporter.config-map" -}}
{{- printf "%s-%s" .Release.Name "dcgm-metrics" | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Effective cloud provider for nodemon.
Derived from the shared global.k8sProvider (set by the parent zxporter chart),
falling back to the chart-local .Values.provider for standalone installs.
Only "gcp" changes behavior (GPU/DCGM wiring); any other value is treated as non-GCP.
*/}}
{{- define "zxporter-nodemon.provider" -}}
{{- $global := .Values.global | default dict -}}
{{- $global.k8sProvider | default .Values.provider | default "" -}}
{{- end }}

{{/*
Create the name of the zxporter-nodemon config map
*/}}
{{- define "zxporter-nodemon.config-map" -}}
{{- printf "%s-%s" .Release.Name "zxporter-nodemon" | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}