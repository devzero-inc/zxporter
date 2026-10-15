{{/*
Service account name: the created SA's name when serviceAccount.create, else the
referenced name (default "default"). Usage:
  serviceAccountName: {{ include "devzero-common.serviceAccount.name" . }}
*/}}
{{- define "devzero-common.serviceAccount.name" -}}
{{- $sa := .Values.serviceAccount | default dict -}}
{{- if $sa.create -}}
{{- $sa.name | default (include "devzero-common.fullname" .) -}}
{{- else -}}
{{- $sa.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/*
Full ServiceAccount object, emitted only when serviceAccount.create is true.
Put this in the consumer's templates/serviceaccount.yaml as the whole body:
  {{ include "devzero-common.serviceAccount" . }}
*/}}
{{- define "devzero-common.serviceAccount" -}}
{{- $sa := .Values.serviceAccount | default dict -}}
{{- if $sa.create -}}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "devzero-common.serviceAccount.name" . }}
  labels:
    {{- include "devzero-common.labels" . | nindent 4 }}
  {{- with $sa.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- /* hasKey, not `default true`: sprig's default treats boolean false as empty,
     so `serviceAccount.automount: false` — the security-hardening case — would
     be silently flipped back to true and be impossible to turn off. */}}
{{- if hasKey $sa "automount" }}
automountServiceAccountToken: {{ $sa.automount }}
{{- else }}
automountServiceAccountToken: true
{{- end }}
{{- end -}}
{{- end -}}
