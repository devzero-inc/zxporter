{{/*
Container resources: .Values.resources verbatim, or a conservative default when
unset. Usage:  resources: {{- include "devzero-common.resources" . | nindent 12 }}
*/}}
{{- define "devzero-common.resources" -}}
{{- if .Values.resources -}}
{{- toYaml .Values.resources -}}
{{- else -}}
requests:
  cpu: 100m
  memory: 128Mi
limits:
  memory: 256Mi
{{- end -}}
{{- end -}}

{{/*
OTLP telemetry env vars, injected from the umbrella's global.telemetry.otlp
({enabled, endpoint}). Emits nothing when disabled/unset. Placed under a
container's env:.  Usage:  env: {{- include "devzero-common.telemetry" . | nindent 12 }}
*/}}
{{- define "devzero-common.telemetry" -}}
{{- $otlp := ((.Values.global | default dict).telemetry | default dict).otlp | default dict -}}
{{- if $otlp.enabled -}}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ required "global.telemetry.otlp.endpoint is required when otlp.enabled" $otlp.endpoint | quote }}
- name: OTEL_SERVICE_NAME
  value: {{ include "devzero-common.name" . | quote }}
{{- end -}}
{{- end -}}
