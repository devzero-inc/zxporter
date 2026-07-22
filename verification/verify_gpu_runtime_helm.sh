#!/bin/sh

set -eu

cd "$(dirname "$0")/.."

HELM_BIN=${HELM_BIN:-./bin/helm}
if [ ! -x "$HELM_BIN" ]; then
  HELM_BIN=$(command -v helm || true)
fi

YQ_BIN=${YQ_BIN:-./bin/yq}
if [ ! -x "$YQ_BIN" ]; then
  YQ_BIN=$(command -v yq || true)
fi

if [ -z "$HELM_BIN" ]; then
  make -s helm
  HELM_BIN=./bin/helm
fi

if [ -z "$YQ_BIN" ]; then
  make -s yq
  YQ_BIN=./bin/yq
fi

if [ ! -x "$HELM_BIN" ]; then
  echo "helm not found; run 'make helm' first" >&2
  exit 1
fi

if [ ! -x "$YQ_BIN" ]; then
  echo "yq not found; run 'make yq' first" >&2
  exit 1
fi

manifest=$(mktemp "${TMPDIR:-/tmp}/zxporter-gpu-runtime-helm.XXXXXX")
stderr=$(mktemp "${TMPDIR:-/tmp}/zxporter-gpu-runtime-helm-stderr.XXXXXX")
cleanup() {
  rm -f "$manifest" "$stderr"
}
trap cleanup EXIT HUP INT TERM

assert_eq() {
  description=$1
  actual=$2
  expected=$3

  if [ "$actual" != "$expected" ]; then
    echo "Helm verification failed: $description (expected '$expected', got '$actual')" >&2
    exit 1
  fi
}

assert_render_value() {
  description=$1
  expr=$2
  expected=$3
  actual=$("$YQ_BIN" eval --unwrapScalar "$expr" "$manifest")
  assert_eq "$description" "$actual" "$expected"
}

assert_render_absent() {
  description=$1
  expr=$2
  actual=$("$YQ_BIN" eval --unwrapScalar "$expr" "$manifest")

  if [ -n "$actual" ] && [ "$actual" != "null" ]; then
    echo "Helm verification failed: $description (expected field to be absent, got '$actual')" >&2
    exit 1
  fi
}

expect_render_failure() {
  description=$1
  expected_message=$2
  shift 2

  if "$@" >"$manifest" 2>"$stderr"; then
    echo "Helm verification failed: $description (expected render failure)" >&2
    exit 1
  fi

  if ! grep -Fq "$expected_message" "$stderr"; then
    echo "Helm verification failed: $description (stderr did not include '$expected_message')" >&2
    cat "$stderr" >&2
    exit 1
  fi
}

render_parent() {
  "$HELM_BIN" template test helm-chart/zxporter --namespace devzero-system \
    --set global.k8sProvider=other \
    --set zxporter.clusterToken=test \
    --set zxporter.kubeContextName=test \
    "$@"
}

render_nodemon() {
  "$HELM_BIN" template test helm-chart/zxporter-nodemon --namespace devzero-system \
    --set global.k8sProvider=other \
    "$@"
}

render_parent >"$manifest"
assert_render_value "parent auto GPU runtime mode" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .metadata.annotations."devzero.io/gpu-runtime-mode"' "auto"
assert_render_value "parent auto GPU runtime class annotation" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .metadata.annotations."devzero.io/gpu-runtime-class"' "nvidia"
assert_render_value "parent auto GPU runtimeClassName" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .spec.template.spec.runtimeClassName' "nvidia"
assert_render_value "parent auto base affinity operator" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon") | .spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator' "NotIn"
assert_render_value "parent auto gpu affinity operator" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator' "In"
assert_render_value "parent auto resolver reader role" 'select(.kind == "ClusterRole" and .metadata.name == "devzero-zxporter-gpu-runtime-reader") | .rules[0].resourceNames[0]' "nvidia"
assert_render_value "parent auto resolver patcher role" 'select(.kind == "Role" and .metadata.name == "devzero-zxporter-gpu-runtime-patcher") | .rules[0].resourceNames[0]' "zxporter-nodemon-gpu"

render_parent --set global.gpuRuntime.mode=default >"$manifest"
assert_render_value "parent default GPU runtime mode" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .metadata.annotations."devzero.io/gpu-runtime-mode"' "default"
assert_render_value "parent default GPU runtime class annotation length" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .metadata.annotations."devzero.io/gpu-runtime-class" | length' "0"
assert_render_absent "parent default GPU runtimeClassName" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .spec.template.spec.runtimeClassName'
assert_render_absent "parent default resolver reader role" 'select(.kind == "ClusterRole" and .metadata.name == "devzero-zxporter-gpu-runtime-reader") | .metadata.name'
assert_render_absent "parent default resolver patcher role" 'select(.kind == "Role" and .metadata.name == "devzero-zxporter-gpu-runtime-patcher") | .metadata.name'

render_parent --set global.gpuRuntime.mode=explicit --set global.gpuRuntime.runtimeClassName=nvidia-cdi >"$manifest"
assert_render_value "parent explicit GPU runtime mode" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .metadata.annotations."devzero.io/gpu-runtime-mode"' "explicit"
assert_render_value "parent explicit GPU runtime class annotation" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .metadata.annotations."devzero.io/gpu-runtime-class"' "nvidia-cdi"
assert_render_value "parent explicit GPU runtimeClassName" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .spec.template.spec.runtimeClassName' "nvidia-cdi"
assert_render_absent "parent explicit resolver reader role" 'select(.kind == "ClusterRole" and .metadata.name == "devzero-zxporter-gpu-runtime-reader") | .metadata.name'
assert_render_absent "parent explicit resolver patcher role" 'select(.kind == "Role" and .metadata.name == "devzero-zxporter-gpu-runtime-patcher") | .metadata.name'

expect_render_failure "parent explicit empty runtimeClassName" "global.gpuRuntime.runtimeClassName is required when global.gpuRuntime.mode is explicit." \
  render_parent --set global.gpuRuntime.mode=explicit --set-string global.gpuRuntime.runtimeClassName=

expect_render_failure "parent invalid gpu runtime mode" "global.gpuRuntime.mode must be one of: auto, default, explicit." \
  render_parent --set global.gpuRuntime.mode=banana

render_nodemon >"$manifest"
assert_render_value "standalone default GPU runtime mode" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .metadata.annotations."devzero.io/gpu-runtime-mode"' "explicit"
assert_render_value "standalone default GPU runtime class annotation" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .metadata.annotations."devzero.io/gpu-runtime-class"' "nvidia"
assert_render_value "standalone default GPU runtimeClassName" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .spec.template.spec.runtimeClassName' "nvidia"
assert_render_value "standalone base affinity operator" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon") | .spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator' "NotIn"
assert_render_value "standalone gpu affinity operator" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator' "In"
assert_render_absent "standalone resolver reader role" 'select(.kind == "ClusterRole" and .metadata.name == "devzero-zxporter-gpu-runtime-reader") | .metadata.name'
assert_render_absent "standalone resolver patcher role" 'select(.kind == "Role" and .metadata.name == "devzero-zxporter-gpu-runtime-patcher") | .metadata.name'

render_nodemon --set-string dcgmExporter.runtimeClassName= >"$manifest"
assert_render_value "standalone legacy disable keeps base daemonset" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon") | .metadata.name' "zxporter-nodemon"
assert_render_absent "standalone legacy disable removes GPU daemonset" 'select(.kind == "DaemonSet" and .metadata.name == "zxporter-nodemon-gpu") | .metadata.name'

echo "GPU runtime Helm verification passed."
