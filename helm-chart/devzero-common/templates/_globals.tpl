{{/*
Cluster identity the platform umbrella sets once for every component:

  global.clusterName  the name dakr shows for the cluster
  global.dakrUrl      the dakr endpoint the component reports to
  global.k8sProvider  the cloud provider (aws, gcp, azure, oci, other)

Each helper takes (list $ <the chart's own value>) and returns the global when
it is set, else the chart's own value, so the umbrella overrides every
component in one place (as global.imageRegistry does) and a standalone install
keeps using the key it always had. The result is a plain string; quote it at
the call site.

Usage: KUBE_CONTEXT_NAME: {{ include "devzero-common.clusterName" (list . .Values.zxporter.kubeContextName) | quote }}
*/}}
{{- define "devzero-common.clusterName" -}}
{{- include "devzero-common.globalOr" (list (index . 0) "clusterName" (index . 1)) -}}
{{- end -}}

{{- define "devzero-common.dakrUrl" -}}
{{- include "devzero-common.globalOr" (list (index . 0) "dakrUrl" (index . 1)) -}}
{{- end -}}

{{- define "devzero-common.k8sProvider" -}}
{{- include "devzero-common.globalOr" (list (index . 0) "k8sProvider" (index . 1)) -}}
{{- end -}}

{{/*
global.<key> unless it is null or "", else the given value, else "". Only null
and "" count as unset: Helm's default would also skip 0, and a cluster named 0
must not silently report as the chart's own name. Not for direct use: call the
named helpers above, which fix the set of keys.
*/}}
{{- define "devzero-common.globalOr" -}}
{{- $ctx := index . 0 -}}
{{- $g := $ctx.Values.global | default dict -}}
{{- $v := index $g (index . 1) -}}
{{- if or (kindIs "invalid" $v) (eq (toString $v) "") -}}
{{- $v = index . 2 -}}
{{- end -}}
{{- if kindIs "invalid" $v }}{{ else }}{{ $v }}{{ end -}}
{{- end -}}
