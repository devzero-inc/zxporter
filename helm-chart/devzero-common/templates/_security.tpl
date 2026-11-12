{{/*
cert-manager Certificate, emitted only when the umbrella sets
global.tls.certManagerIssuerRef. The issued secret is named
<fullname>-tls and can be mounted by the consumer. Put in the consumer's
templates/certificate.yaml as the whole body:  {{ include "devzero-common.tls.certificate" . }}
*/}}
{{- define "devzero-common.tls.certificate" -}}
{{- $tls := (.Values.global | default dict).tls | default dict -}}
{{- with $tls.certManagerIssuerRef }}
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: {{ include "devzero-common.fullname" $ }}-tls
  labels:
    {{- include "devzero-common.labels" $ | nindent 4 }}
spec:
  secretName: {{ include "devzero-common.fullname" $ }}-tls
  issuerRef:
    {{- toYaml . | nindent 4 }}
  dnsNames:
    - {{ include "devzero-common.fullname" $ }}
    - {{ include "devzero-common.fullname" $ }}.{{ $.Release.Namespace }}.svc
{{- end }}
{{- end -}}

{{/*
Secret name resolver: the caller's existing secret when secret.existingSecret is
set, otherwise the chart's own <fullname>. Lets a consumer BYO-secret or create
one inline without duplicating the name logic.
Usage: secretKeyRef: { name: {{ include "devzero-common.secretName" . }}, key: ... }
*/}}
{{- define "devzero-common.secretName" -}}
{{- $s := .Values.secret | default dict -}}
{{- $s.existingSecret | default (include "devzero-common.fullname" .) -}}
{{- end -}}

{{/*
True (string "true") when this chart should create the inline Secret itself —
i.e. no existingSecret was provided. Usage:  {{- if eq (include "devzero-common.createSecret" .) "true" }}
*/}}
{{- define "devzero-common.createSecret" -}}
{{- $s := .Values.secret | default dict -}}
{{- if $s.existingSecret -}}false{{- else -}}true{{- end -}}
{{- end -}}
