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
{{- with (include "zxporter-nodemon.commonLabels" .) }}
{{ . }}
{{- end }}
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

{{/*
Validate required configuration.
*/}}
{{- define "zxporter-nodemon.validateConfig" -}}
{{- $gpuRuntimeMode := default "explicit" (dig "gpuRuntime" "mode" "" .Values.global) -}}
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
Renders nothing when both are unset, so call sites guard with `with`.
*/}}
{{- define "zxporter-nodemon.commonLabels" -}}
{{- $extra := deepCopy (.Values.commonLabels | default dict) -}}
{{- if .Values.global }}{{- with .Values.global.commonLabels }}{{- $extra = merge $extra (deepCopy .) }}{{- end }}{{- end }}
{{- /* Completeness of this list is enforced by the "no chart-emitted key can
       produce a duplicate" assertion in helm-chart/internal/charttest, which
       derives the real set from a render rather than restating it here. */}}
{{- range $key := list "helm.sh/chart" "app.kubernetes.io/name" "app.kubernetes.io/instance" "app.kubernetes.io/version" "app.kubernetes.io/managed-by" "app.kubernetes.io/component" "app" "control-plane" }}
{{- $_ := unset $extra $key }}
{{- end }}
{{- with $extra }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/*
Site-wide annotations for object metadata.
*/}}
{{- define "zxporter-nodemon.commonAnnotations" -}}
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
{{- define "zxporter-nodemon.podExtraLabels" -}}
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
{{- define "zxporter-nodemon.podExtraAnnotations" -}}
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
