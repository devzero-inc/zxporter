# zxporter release automation.
# `just tag zxporter vX.Y.Z` validates + pushes ONE tag; CI (release.yml) does the rest:
# builds/pushes the 3 images, publishes the 3 OCI charts at a unified version, opens one
# auto-merged bump PR (Chart.yaml/lock, values.yaml, dist/), cuts the GitHub Release, and
# dispatches the cross-repo services metadata/manifest update.

set shell := ["bash", "-uc"]

_default:
    @just --list

# Show recent release + chart tags.
tag-list:
    @git tag --sort=-creatordate | grep -E '^(v|chart-)' | head -n 20

# Cut a unified release. Dispatches the Release workflow, which bumps versions, tags the
# bumped commit, builds/pushes images + charts, opens the bump PR, and updates services.
# Usage: just tag zxporter v0.1.0     (append `dry` to preview: just tag zxporter v0.1.0 dry)
tag component version dry="":
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ "{{component}}" != "zxporter" ]]; then
      echo "error: only 'zxporter' is supported (got '{{component}}')" >&2; exit 1
    fi
    if [[ ! "{{version}}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
      echo "error: version must match vX.Y.Z (got '{{version}}')" >&2; exit 1
    fi
    if git ls-remote --exit-code --tags origin "{{version}}" >/dev/null 2>&1; then
      echo "error: tag {{version}} already exists on origin" >&2; exit 1
    fi
    if [[ "{{dry}}" == "dry" ]]; then
      echo "[dry-run] would: gh workflow run release.yml --repo devzero-inc/zxporter -f version={{version}}"
      exit 0
    fi
    gh workflow run release.yml --repo devzero-inc/zxporter -f version="{{version}}"
    echo "Dispatched release {{version}}. Watch: https://github.com/devzero-inc/zxporter/actions/workflows/release.yml"
