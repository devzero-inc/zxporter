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
Create a default fully qualified app name (truncated to 63 chars for the DNS naming spec).
NOTE: unlike the stock Helm helper, this intentionally does NOT incorporate .Release.Name. This chart
is a cluster singleton — the zxporter controller discovers it via the fixed label
app.kubernetes.io/name=zxporter-nodemon, and the generated dist/ bundles hardcode the name
"zxporter-nodemon". A release-name-derived name would break that parity and the immutable DaemonSet
selector, so the name defaults to the chart name and only changes via fullnameOverride/nameOverride.
*/}}
{{- define "zxporter-nodemon.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Selector labels.
NOTE: app.kubernetes.io/instance is pinned to the stable fullname (not .Release.Name) because it is
part of the IMMUTABLE DaemonSet selector. This keeps `helm install <any-release>` byte-identical to
the generated dist/ bundles, so dist/installer_updater.yaml patches the existing DaemonSet instead
of failing on an immutable-selector change.
*/}}
{{- define "zxporter-nodemon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zxporter-nodemon.name" . }}
app.kubernetes.io/instance: {{ include "zxporter-nodemon.fullname" . }}
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
{{- printf "%s-%s" (include "zxporter-nodemon.fullname" .) "dcgm-metrics" | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create the name of the zxporter-nodemon config map
*/}}
{{- define "zxporter-nodemon.config-map" -}}
{{- printf "%s-%s" (include "zxporter-nodemon.fullname" .) "zxporter-nodemon" | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}