{{/*
Image reference: "<registry>/<repository>:<tag>". registry falls back
global.imageRegistry -> image.registry (umbrella override wins); tag falls back
image.tag -> .Chart.AppVersion -> .Chart.Version. repository is required — the
schema forbids `image` as a bare string, so image.repository always exists.
Usage: image: {{ include "devzero-common.image" . }}
*/}}
{{- define "devzero-common.image" -}}
{{- $g := .Values.global | default dict -}}
{{- $img := .Values.image | default dict -}}
{{- $registry := $g.imageRegistry | default $img.registry -}}
{{- $repository := required "image.repository is required (image must be an object, not a string)" $img.repository -}}
{{- $tag := $img.tag | default .Chart.AppVersion | default .Chart.Version -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry $repository $tag -}}
{{- else -}}
{{- printf "%s:%s" $repository $tag -}}
{{- end -}}
{{- end -}}

{{/*
Merged image pull secrets (global.imagePullSecrets + image.pullSecrets), deduped.
Emits the whole `imagePullSecrets:` block, or nothing when empty. Accepts both
["name"] and [{name: "..."}] element shapes.
Usage: {{- include "devzero-common.imagePullSecrets" . | nindent 6 }}
*/}}
{{- define "devzero-common.imagePullSecrets" -}}
{{- $g := .Values.global | default dict -}}
{{- $img := .Values.image | default dict -}}
{{- /* Normalize both accepted element shapes down to plain names BEFORE
     deduping. Deduping the raw list would miss the common case where the same
     secret arrives as "foo" from one source and {name: foo} from the other —
     uniq compares whole elements, so those two never match. */ -}}
{{- $names := list -}}
{{- range concat ($g.imagePullSecrets | default list) ($img.pullSecrets | default list) -}}
{{- if kindIs "map" . -}}
{{- /* Not `default`: it would drop a name that is 0 or false. */ -}}
{{- if not (kindIs "invalid" .name) -}}
{{- $names = append $names (toString .name) -}}
{{- end -}}
{{- else if not (kindIs "invalid" .) -}}
{{- $names = append $names (toString .) -}}
{{- end -}}
{{- end -}}
{{- /* Names are strings before deduping (--set x[0].name=123 arrives as an
     int), and are emitted through toYaml, which quotes a name YAML would
     otherwise read as another type (yes, on, null, true, 123) and leaves
     every other name bare. */ -}}
{{- with ($names | compact | uniq) -}}
imagePullSecrets:
{{- range . }}
  - name: {{ toYaml . }}
{{- end }}
{{- end -}}
{{- end -}}

{{/*
A full image reference with its registry host swapped for global.imageRegistry,
for charts that build references from many keys instead of one `image` object.
With global.imageRegistry empty the reference is returned unchanged, so an
install that does not set it renders exactly as before.

When set, the result is "<global.imageRegistry>/<path>[:tag][@digest]", where
<path> is the reference without its registry host. That is the layout
`crane copy`, `skopeo sync` and `oras copy` produce when copying a list of
images into one registry, so a mirror needs no rename table. Parsing follows
the Docker reference grammar (distribution/reference):

  1. Split off the digest (@...), then the tag: the last ":" after the last "/".
  2. Only a name containing "/" can carry a host, and its first component is
     the host when it contains "." or ":", is "localhost", or has an
     uppercase letter (path components must be lowercase). So the ":" in
     alpine:3.19 is a tag, and localhost:5000/x has the host localhost:5000.
  3. A Docker Hub name with a single component gets "library/" (alpine:3.19
     and docker.io/alpine:3.19 become <registry>/library/alpine:3.19), as
     Docker Hub stores it.

Tags and digests pass through unchanged, so digest pins stay verifiable.
global.imageRegistry may carry a path prefix (harbor.corp/devzero); a trailing
"/" is ignored. An empty reference stays empty.

Usage: image: {{ include "devzero-common.imageRef" (dict "ref" $ref "global" .Values.global) | quote }}
*/}}
{{- define "devzero-common.imageRef" -}}
{{- $ref := .ref | default "" | toString -}}
{{- $g := .global | default dict -}}
{{- $registry := $g.imageRegistry | default "" | toString | trimSuffix "/" -}}
{{- if or (not $registry) (not $ref) -}}
{{- $ref -}}
{{- else -}}
{{- $name := $ref -}}
{{- $digest := "" -}}
{{- if contains "@" $name -}}
{{- $parts := splitn "@" 2 $name -}}
{{- $name = $parts._0 -}}
{{- $digest = printf "@%s" $parts._1 -}}
{{- end -}}
{{- $tag := regexFind ":[^:/]+$" $name -}}
{{- $name = trimSuffix $tag $name -}}
{{- $path := $name -}}
{{- $dockerHub := true -}}
{{- if contains "/" $name -}}
{{- $parts := splitn "/" 2 $name -}}
{{- $first := $parts._0 -}}
{{- if or (contains "." $first) (contains ":" $first) (eq $first "localhost") (ne (lower $first) $first) -}}
{{- $path = $parts._1 -}}
{{- $dockerHub = has $first (list "docker.io" "index.docker.io" "registry-1.docker.io") -}}
{{- end -}}
{{- end -}}
{{- if and $dockerHub (not (contains "/" $path)) -}}
{{- $path = printf "library/%s" $path -}}
{{- end -}}
{{- printf "%s/%s%s%s" $registry $path $tag $digest -}}
{{- end -}}
{{- end -}}
