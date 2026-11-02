#!/usr/bin/env bash
#
# kind-preload-images.sh — pull images once on the CI runner and import them
# into every node of a kind cluster, so the kubelets never pull them from the
# registry themselves.
#
#   scripts/kind-preload-images.sh <cluster> <image-ref>...
#   helm template ... | scripts/kind-preload-images.sh <cluster> --from-manifest <registry-prefix>
#
# KIND names the kind binary (default: kind on PATH).
#
# Why: public.ecr.aws limits anonymous pulls per source IP ("429 Too Many
# Requests ... toomanyrequests: Data limit exceeded"), and the CI runners share
# egress IPs. Every node of every kind cluster in every matrix leg pulled the
# charts' digest-pinned hook image on its own, so a busy day failed Helm hooks
# with ImagePullBackOff. Pulling once on the runner (authenticated when the
# job has logged in to public.ecr.aws, retried on failure) and importing the
# result takes the cluster off the registry entirely.
#
# Why not `kind load docker-image`: it imports with --all-platforms, which
# fails for a multi-arch image the runner pulled for one platform ("content
# digest ... not found"), and it cannot name an image referenced by digest, so
# the kubelet still pulls `repo@sha256:...`. This imports the node's platform
# only and, for a digest reference, names the imported index `repo@sha256:...`
# (ctr --digests --base-name), the exact name the kubelet looks up. The pod
# must use imagePullPolicy IfNotPresent (or Never): with Always the kubelet
# asks the registry again regardless.
#
# Every reference is checked on every node with `crictl inspecti` afterwards,
# so a reference the kubelet would not find fails here, not as a hook timeout.
#
# A copy of the services monorepo's scripts/kind-preload-images.sh, carried
# here so the zxporter tree (and its public repository) stays self-contained.
# Keep the two in step.

set -euo pipefail

usage() {
  echo "usage: $0 <kind-cluster> <image-ref>..." >&2
  echo "       $0 <kind-cluster> --from-manifest <registry-prefix>  (manifest on stdin)" >&2
  exit 2
}

[ "$#" -ge 2 ] || usage
cluster="$1"
shift

refs=()
if [ "$1" = "--from-manifest" ]; then
  [ "$#" -eq 2 ] || usage
  prefix="$2"
  # `image: <ref>`, `image: "<ref>"` and `- image: <ref>`, as Helm renders them.
  while IFS= read -r ref; do
    refs+=("$ref")
  done < <(sed -nE 's/^[[:space:]-]*image:[[:space:]]*"?([^"[:space:]]+)"?[[:space:]]*$/\1/p' |
    grep -F "${prefix}/" | sort -u)
  if [ "${#refs[@]}" -eq 0 ]; then
    echo "kind-preload-images: the manifest references no ${prefix}/ image; nothing to preload"
    exit 0
  fi
else
  refs=("$@")
fi

nodes=()
while IFS= read -r node; do
  nodes+=("$node")
done < <("${KIND:-kind}" get nodes --name "$cluster")
[ "${#nodes[@]}" -gt 0 ] || {
  echo "::error::kind cluster '$cluster' has no nodes" >&2
  exit 1
}

# pull_with_retry <ref>: a 429 is transient for a per-second limit and the
# runner may land on another egress IP, so back off and try again.
pull_with_retry() {
  local ref="$1" attempt delay=10
  for attempt in 1 2 3 4 5 6; do
    if docker pull --quiet "$ref"; then
      return 0
    fi
    if [ "$attempt" -eq 6 ]; then
      echo "::error::could not pull $ref after $attempt attempts" >&2
      return 1
    fi
    local wait=$((delay + RANDOM % 10))
    echo "::warning::pull of $ref failed (attempt $attempt); retrying in ${wait}s"
    sleep "$wait"
    delay=$((delay * 2))
  done
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

for ref in "${refs[@]}"; do
  echo "Preloading $ref into kind cluster $cluster (${nodes[*]})"
  pull_with_retry "$ref"
  archive="$tmp/image.tar"
  docker save "$ref" -o "$archive"

  import_args=(--namespace=k8s.io images import)
  case "$ref" in
    *@sha256:*)
      # Name the imported index repo@sha256:<digest>: that digest is the
      # index digest only when the runner's Docker keeps registry manifests
      # (the containerd image store); the crictl check below catches a
      # runner that does not.
      import_args+=(--digests --base-name "${ref%@*}")
      ;;
  esac

  for node in "${nodes[@]}"; do
    docker exec -i "$node" ctr "${import_args[@]}" - <"$archive" >/dev/null
    if ! docker exec "$node" crictl inspecti "$ref" >/dev/null 2>&1; then
      echo "::error::$ref was imported into $node but the kubelet cannot find it by that reference" >&2
      docker exec "$node" ctr --namespace=k8s.io images ls -q >&2 || true
      exit 1
    fi
  done
  rm -f "$archive"
  echo "ok: $ref on ${#nodes[@]} node(s)"
done
