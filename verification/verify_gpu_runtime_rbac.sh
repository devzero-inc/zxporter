#!/bin/sh

set -eu

cd "$(dirname "$0")/.."

KUSTOMIZE_BIN=${KUSTOMIZE_BIN:-./bin/kustomize}
if [ ! -x "$KUSTOMIZE_BIN" ]; then
  KUSTOMIZE_BIN=$(command -v kustomize || true)
fi

YQ_BIN=${YQ_BIN:-./bin/yq}
if [ ! -x "$YQ_BIN" ]; then
  YQ_BIN=$(command -v yq || true)
fi

if [ -z "$YQ_BIN" ]; then
  make -s yq
  YQ_BIN=./bin/yq
fi

if [ -z "$KUSTOMIZE_BIN" ]; then
  echo "kustomize not found; run 'make kustomize' first" >&2
  exit 1
fi

if [ -z "$YQ_BIN" ]; then
  echo "yq not found; run 'make yq' first" >&2
  exit 1
fi

manifest=$(mktemp "${TMPDIR:-/tmp}/zxporter-gpu-runtime-rbac.XXXXXX")
trap 'rm -f "$manifest"' EXIT HUP INT TERM

"$KUSTOMIZE_BIN" build config/default >"$manifest"

verify_value() {
  description=$1
  expression=$2
  expected=$3
  actual=$("$YQ_BIN" eval --unwrapScalar "$expression" "$manifest")

  if [ "$actual" != "$expected" ]; then
    echo "RBAC verification failed: $description (expected '$expected', got '$actual')" >&2
    exit 1
  fi
}

contains_line() (
  values=$1
  expected=$2
  set -f
  IFS='
'

  for value in $values; do
    if [ "$value" = "$expected" ]; then
      return 0
    fi
  done

  return 1
)

verify_no_unsafe_resolver_grants() {
  bound_roles=$("$YQ_BIN" eval --no-doc '
    select(.kind == "ClusterRoleBinding" or .kind == "RoleBinding") as $binding |
    $binding.subjects[] |
    select(
      .kind == "ServiceAccount" and
      .name == "devzero-zxporter-controller-manager" and
      .namespace == "devzero-system"
    ) |
    $binding.roleRef.kind + "|" + $binding.roleRef.name
  ' "$manifest")

  previous_ifs=$IFS
  IFS='
'

  for bound_role in $bound_roles; do
    role_kind=${bound_role%%|*}
    role_name=${bound_role#*|}
    role="select(.apiVersion == \"rbac.authorization.k8s.io/v1\" and .kind == \"$role_kind\" and .metadata.name == \"$role_name\")"
    rule_count=$("$YQ_BIN" eval --unwrapScalar "$role | .rules | length" "$manifest")
    rule_index=0

    while [ "$rule_index" -lt "$rule_count" ]; do
      api_groups=$("$YQ_BIN" eval --no-doc "$role | .rules[$rule_index].apiGroups[]" "$manifest")
      resources=$("$YQ_BIN" eval --no-doc "$role | .rules[$rule_index].resources[]" "$manifest")
      verbs=$("$YQ_BIN" eval --no-doc "$role | .rules[$rule_index].verbs[]" "$manifest")
      api_group_count=$("$YQ_BIN" eval --unwrapScalar "$role | .rules[$rule_index].apiGroups | length" "$manifest")
      resource_count=$("$YQ_BIN" eval --unwrapScalar "$role | .rules[$rule_index].resources | length" "$manifest")
      resource_name_count=$("$YQ_BIN" eval --unwrapScalar "$role | .rules[$rule_index].resourceNames | length" "$manifest")
      verb_count=$("$YQ_BIN" eval --unwrapScalar "$role | .rules[$rule_index].verbs | length" "$manifest")

      if { contains_line "$api_groups" "node.k8s.io" || contains_line "$api_groups" "*"; } &&
         { contains_line "$resources" "runtimeclasses" || contains_line "$resources" "*"; }; then
        resource_names=$("$YQ_BIN" eval --no-doc "$role | .rules[$rule_index].resourceNames[]" "$manifest")

        if [ "$api_group_count" -ne 1 ] || ! contains_line "$api_groups" "node.k8s.io" ||
           [ "$resource_count" -ne 1 ] || ! contains_line "$resources" "runtimeclasses" ||
           [ "$resource_name_count" -ne 1 ] || ! contains_line "$resource_names" "nvidia" ||
           [ "$verb_count" -ne 1 ] || ! contains_line "$verbs" "get"; then
          echo "RBAC verification failed: $role_kind/$role_name grants RuntimeClass access broader than get on nvidia" >&2
          exit 1
        fi
      fi

      if { contains_line "$api_groups" "apps" || contains_line "$api_groups" "*"; } &&
         { contains_line "$resources" "daemonsets" || contains_line "$resources" "*"; } &&
         { contains_line "$verbs" "create" || contains_line "$verbs" "delete" ||
           contains_line "$verbs" "deletecollection" || contains_line "$verbs" "update" ||
           contains_line "$verbs" "patch" || contains_line "$verbs" "*"; }; then
        resource_names=$("$YQ_BIN" eval --no-doc "$role | .rules[$rule_index].resourceNames[]" "$manifest")

        if [ "$api_group_count" -ne 1 ] || ! contains_line "$api_groups" "apps" ||
           [ "$resource_count" -ne 1 ] || ! contains_line "$resources" "daemonsets" ||
           [ "$resource_name_count" -ne 1 ] || ! contains_line "$resource_names" "zxporter-nodemon-gpu" ||
           contains_line "$verbs" "create" || contains_line "$verbs" "delete" ||
           contains_line "$verbs" "deletecollection" || contains_line "$verbs" "update" ||
           contains_line "$verbs" "*" ||
           ! contains_line "$verbs" "patch"; then
          echo "RBAC verification failed: $role_kind/$role_name grants DaemonSet write access broader than patch on zxporter-nodemon-gpu" >&2
          exit 1
        fi
      fi

      rule_index=$((rule_index + 1))
    done
  done

  IFS=$previous_ifs
}

reader='select(.apiVersion == "rbac.authorization.k8s.io/v1" and .kind == "ClusterRole" and .metadata.name == "devzero-zxporter-gpu-runtime-reader")'
verify_value "RuntimeClass reader role must exist exactly once" "$reader | .metadata.name" "devzero-zxporter-gpu-runtime-reader"
verify_value "RuntimeClass reader must contain exactly one rule" "$reader | .rules | length" "1"
verify_value "RuntimeClass reader must contain exactly one API group" "$reader | .rules[0].apiGroups | length" "1"
verify_value "RuntimeClass reader API group" "$reader | .rules[0].apiGroups[0]" "node.k8s.io"
verify_value "RuntimeClass reader must contain exactly one resource" "$reader | .rules[0].resources | length" "1"
verify_value "RuntimeClass reader resource" "$reader | .rules[0].resources[0]" "runtimeclasses"
verify_value "RuntimeClass reader get must be resource-name restricted" "$reader | .rules[0].resourceNames | length" "1"
verify_value "RuntimeClass reader resource name" "$reader | .rules[0].resourceNames[0]" "nvidia"
verify_value "RuntimeClass reader must contain exactly one verb" "$reader | .rules[0].verbs | length" "1"
verify_value "RuntimeClass reader verb" "$reader | .rules[0].verbs[0]" "get"

patcher='select(.apiVersion == "rbac.authorization.k8s.io/v1" and .kind == "Role" and .metadata.name == "devzero-zxporter-gpu-runtime-patcher")'
verify_value "DaemonSet patcher role must exist exactly once" "$patcher | .metadata.name" "devzero-zxporter-gpu-runtime-patcher"
verify_value "DaemonSet patcher namespace" "$patcher | .metadata.namespace" "devzero-system"
verify_value "DaemonSet patcher must contain exactly one rule" "$patcher | .rules | length" "1"
verify_value "DaemonSet patcher must contain exactly one API group" "$patcher | .rules[0].apiGroups | length" "1"
verify_value "DaemonSet patcher API group" "$patcher | .rules[0].apiGroups[0]" "apps"
verify_value "DaemonSet patcher must contain exactly one resource" "$patcher | .rules[0].resources | length" "1"
verify_value "DaemonSet patcher resource" "$patcher | .rules[0].resources[0]" "daemonsets"
verify_value "DaemonSet patch must be resource-name restricted" "$patcher | .rules[0].resourceNames | length" "1"
verify_value "DaemonSet patcher resource name" "$patcher | .rules[0].resourceNames[0]" "zxporter-nodemon-gpu"
verify_value "DaemonSet patcher must contain exactly two verbs" "$patcher | .rules[0].verbs | length" "2"
verify_value "DaemonSet patcher first verb" "$patcher | .rules[0].verbs[0]" "get"
verify_value "DaemonSet patcher second verb" "$patcher | .rules[0].verbs[1]" "patch"

reader_binding='select(.apiVersion == "rbac.authorization.k8s.io/v1" and .kind == "ClusterRoleBinding" and .metadata.name == "devzero-zxporter-gpu-runtime-reader-rolebinding")'
verify_value "RuntimeClass reader binding must exist exactly once" "$reader_binding | .metadata.name" "devzero-zxporter-gpu-runtime-reader-rolebinding"
verify_value "RuntimeClass reader roleRef API group" "$reader_binding | .roleRef.apiGroup" "rbac.authorization.k8s.io"
verify_value "RuntimeClass reader roleRef kind" "$reader_binding | .roleRef.kind" "ClusterRole"
verify_value "RuntimeClass reader roleRef name" "$reader_binding | .roleRef.name" "devzero-zxporter-gpu-runtime-reader"
verify_value "RuntimeClass reader binding must contain exactly one subject" "$reader_binding | .subjects | length" "1"
verify_value "RuntimeClass reader subject kind" "$reader_binding | .subjects[0].kind" "ServiceAccount"
verify_value "RuntimeClass reader subject name" "$reader_binding | .subjects[0].name" "devzero-zxporter-controller-manager"
verify_value "RuntimeClass reader subject namespace" "$reader_binding | .subjects[0].namespace" "devzero-system"

patcher_binding='select(.apiVersion == "rbac.authorization.k8s.io/v1" and .kind == "RoleBinding" and .metadata.name == "devzero-zxporter-gpu-runtime-patcher-rolebinding")'
verify_value "DaemonSet patcher binding must exist exactly once" "$patcher_binding | .metadata.name" "devzero-zxporter-gpu-runtime-patcher-rolebinding"
verify_value "DaemonSet patcher binding namespace" "$patcher_binding | .metadata.namespace" "devzero-system"
verify_value "DaemonSet patcher roleRef API group" "$patcher_binding | .roleRef.apiGroup" "rbac.authorization.k8s.io"
verify_value "DaemonSet patcher roleRef kind" "$patcher_binding | .roleRef.kind" "Role"
verify_value "DaemonSet patcher roleRef name" "$patcher_binding | .roleRef.name" "devzero-zxporter-gpu-runtime-patcher"
verify_value "DaemonSet patcher binding must contain exactly one subject" "$patcher_binding | .subjects | length" "1"
verify_value "DaemonSet patcher subject kind" "$patcher_binding | .subjects[0].kind" "ServiceAccount"
verify_value "DaemonSet patcher subject name" "$patcher_binding | .subjects[0].name" "devzero-zxporter-controller-manager"
verify_value "DaemonSet patcher subject namespace" "$patcher_binding | .subjects[0].namespace" "devzero-system"

verify_no_unsafe_resolver_grants

echo "GPU runtime resolver RBAC verified."
