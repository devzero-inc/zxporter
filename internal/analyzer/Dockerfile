# Image Analyzer — Containerd-First Batch Analysis
#
# This image contains dive, crane, ctr (containerd CLI), and jq for
# running batch image analysis on Kubernetes nodes.
#
# Security model:
#   - Runs as root (runAsUser: 0) for containerd socket access
#   - Security enforced via Job securityContext:
#     * allowPrivilegeEscalation: false
#     * Containerd socket mounted read-only
#     * No additional capabilities

FROM alpine:3.21 AS base

RUN apk add --no-cache bash jq coreutils

# crane — for remote registry pulls when containerd export fails
COPY --from=gcr.io/go-containerregistry/crane:latest /ko-app/crane /usr/local/bin/crane

# dive — the core image analysis tool
COPY --from=wagoodman/dive:latest /usr/local/bin/dive /usr/local/bin/dive

# ctr — containerd CLI for local image export
COPY --from=docker.io/rancher/k3s:v1.31.4-k3s1 /bin/ctr /usr/local/bin/ctr

COPY analyze-batch.sh /analyze-batch.sh
RUN chmod +x /analyze-batch.sh

# NOTE: No USER directive — job runs as root (runAsUser: 0) for containerd socket access.
# Security is enforced via: allowPrivilegeEscalation=false, read-only socket mount,
# and no additional capabilities.

ENTRYPOINT ["/analyze-batch.sh"]
