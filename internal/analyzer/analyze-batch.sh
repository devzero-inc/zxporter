#!/bin/bash
set -uo pipefail
# NOTE: intentionally no `set -e` — we handle errors per-image to avoid
# killing the entire batch when a single image analysis fails.

# ============================================================================
# Image Analyzer Batch Script
#
# Input:  IMAGES_JSON env var — JSON array of {digest, ref} objects
#         e.g. [{"digest":"sha256:abc...","ref":"docker.io/nginx:1.25"},...]
#
# Output: One NDJSON line per image to stdout:
#         {"digest":"...","ref":"...","source":"local-containerd|remote-pull|failed",
#          "durationMs":N,"error":"","result":{...}}
#
# The script always exits 0. Per-image failures are encoded in the NDJSON
# output (source="failed" or error field set), never as exit codes.
# ============================================================================

WORKSPACE="${WORKSPACE:-/tmp/workspace}"
CONTAINERD_SOCK="${CONTAINERD_SOCK:-/run/containerd/containerd.sock}"
CONTAINERD_NS="${CONTAINERD_NS:-k8s.io}"
PREFER_LOCAL="${PREFER_LOCAL:-true}"
FALLBACK_REMOTE="${FALLBACK_REMOTE:-true}"
REMOTE_PULL_TIMEOUT="${REMOTE_PULL_TIMEOUT:-300}"

# Ensure workspace exists.
mkdir -p "$WORKSPACE"

# Read IMAGES_JSON from env var, or fall back to file.
IMAGES_JSON="${IMAGES_JSON:-}"
if [ -z "$IMAGES_JSON" ] && [ -f "${WORKSPACE}/images.json" ]; then
    IMAGES_JSON="$(cat "${WORKSPACE}/images.json")"
fi

if [ -z "$IMAGES_JSON" ]; then
    echo '{"digest":"","ref":"","source":"failed","durationMs":0,"error":"IMAGES_JSON is empty and no fallback file found","result":null}'
    exit 0
fi

# Validate IMAGES_JSON is parseable.
if ! echo "$IMAGES_JSON" | jq -e 'type == "array"' >/dev/null 2>&1; then
    echo '{"digest":"","ref":"","source":"failed","durationMs":0,"error":"IMAGES_JSON is not a valid JSON array","result":null}'
    exit 0
fi

# emit_json safely outputs a JSON line, escaping the error string.
# Usage: emit_json "$digest" "$ref" "$source" "$duration_ms" "$error" ["$result_file_path"]
# If result_file_path is provided and exists, reads dive result from file
# to avoid ARG_MAX limits with large JSON payloads.
emit_json() {
    local digest="$1"
    local ref="$2"
    local source="$3"
    local duration_ms="$4"
    local error_msg="$5"
    local result_file="${6:-}"

    if [ -n "$result_file" ] && [ -f "$result_file" ]; then
        # Read result from file using --slurpfile to avoid ARG_MAX limits.
        jq -cn \
            --arg digest "$digest" \
            --arg ref "$ref" \
            --arg source "$source" \
            --argjson durationMs "$duration_ms" \
            --arg error "$error_msg" \
            --slurpfile result "$result_file" \
            '{digest: $digest, ref: $ref, source: $source, durationMs: $durationMs, error: $error, result: $result[0]}'
    else
        # No result file — emit with null result.
        jq -cn \
            --arg digest "$digest" \
            --arg ref "$ref" \
            --arg source "$source" \
            --argjson durationMs "$duration_ms" \
            --arg error "$error_msg" \
            '{digest: $digest, ref: $ref, source: $source, durationMs: $durationMs, error: $error, result: null}'
    fi
}

# get_time_ms returns current time in milliseconds.
get_time_ms() {
    # date +%s%N gives nanoseconds; divide by 1000000 for ms.
    echo $(( $(date +%s%N) / 1000000 ))
}

analyze_image() {
    local digest="$1"
    local ref="$2"
    local tar_path="${WORKSPACE}/image.tar"
    local result_path="${WORKSPACE}/result.json"
    local source="unknown"
    local start_ms
    start_ms=$(get_time_ms)

    # Clean any leftover files from previous iteration.
    rm -f "$tar_path" "$result_path"

    # ---- Strategy 1: Containerd local export ----
    # Uses image ref (not digest) because `ctr images export` needs the tag.
    if [ "$PREFER_LOCAL" = "true" ] && [ -S "$CONTAINERD_SOCK" ]; then
        if ctr -a "$CONTAINERD_SOCK" -n "$CONTAINERD_NS" images export \
            "$tar_path" "$ref" --platform linux/amd64 2>/dev/null; then
            source="local-containerd"
        else
            # ctr may leave behind a partial/corrupt tar on failure.
            # Remove it so the crane fallback can trigger.
            rm -f "$tar_path"
        fi
    fi

    # ---- Strategy 2: Remote pull via crane ----
    # crane reads auth from /root/.docker/config.json (mounted from Secret).
    if [ ! -f "$tar_path" ] && [ "$FALLBACK_REMOTE" = "true" ]; then
        if timeout "$REMOTE_PULL_TIMEOUT" \
            crane pull "$ref" "$tar_path" --platform=linux/amd64 2>/dev/null; then
            source="remote-pull"
        fi
    fi

    # ---- If neither worked, emit error ----
    if [ ! -f "$tar_path" ]; then
        local end_ms
        end_ms=$(get_time_ms)
        local duration_ms=$(( end_ms - start_ms ))
        emit_json "$digest" "$ref" "failed" "$duration_ms" "image acquisition failed: containerd export and remote pull both failed"
        return 0
    fi

    # ---- Run dive analysis ----
    if CI=true dive --source docker-archive "$tar_path" --json "$result_path" 2>/dev/null; then
        local end_ms
        end_ms=$(get_time_ms)
        local duration_ms=$(( end_ms - start_ms ))

        if [ -f "$result_path" ] && [ -s "$result_path" ]; then
            # Validate the dive output is valid JSON before embedding.
            if jq -e . "$result_path" >/dev/null 2>&1; then
                # Pass file path to emit_json; it reads via --slurpfile
                # to avoid ARG_MAX limits with large dive results.
                emit_json "$digest" "$ref" "$source" "$duration_ms" "" "$result_path"
            else
                emit_json "$digest" "$ref" "$source" "$duration_ms" "dive produced invalid JSON output"
            fi
        else
            emit_json "$digest" "$ref" "$source" "$duration_ms" "dive produced empty result file"
        fi
    else
        local exit_code=$?
        local end_ms
        end_ms=$(get_time_ms)
        local duration_ms=$(( end_ms - start_ms ))
        emit_json "$digest" "$ref" "$source" "$duration_ms" "dive analysis failed with exit code $exit_code"
    fi

    # Cleanup tar + result immediately to free disk for the next image.
    rm -f "$tar_path" "$result_path"
    return 0  # always succeed at batch level
}

# ============================================================================
# Main: Process each image in the batch sequentially.
# ============================================================================

image_count=$(echo "$IMAGES_JSON" | jq 'length')
echo "Starting batch analysis of $image_count images" >&2

# Use process substitution instead of pipe to avoid subshell variable scope issues.
index=0
while IFS= read -r img; do
    digest=$(echo "$img" | jq -r '.digest')
    ref=$(echo "$img" | jq -r '.ref')

    echo "[$((index + 1))/$image_count] Analyzing: $ref (digest: ${digest:0:24}...)" >&2
    analyze_image "$digest" "$ref"
    index=$((index + 1))
done < <(echo "$IMAGES_JSON" | jq -c '.[]')

echo "Batch analysis complete" >&2
exit 0  # batch always exits 0; per-image errors are in NDJSON output
