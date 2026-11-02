# Build the manager binary
# The builder runs on the build host's platform and cross-compiles for the
# target (CGO is off), so multi-arch release builds do not run the Go
# toolchain under QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
ENV GOOS=${TARGETOS}
ENV GOARCH=${TARGETARCH}

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.sum ./

# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN --mount=type=cache,id=gomod,target=/go/pkg,sharing=shared \
    go mod download


COPY . .

# Version interface (KUB-151 S7): VERSION is vX.Y.Z[-prerelease], COMMIT the
# short sha, BUILD_DATE RFC 3339. The build step splits VERSION into the
# major/minor/patch the version package reports. The prerelease suffix is
# dropped from patch, as the release workflows did when they passed
# MAJOR/MINOR/PATCH themselves, so a release reports the same version as before.
# GIT_TREE_STATE is not part of the release interface: releases build from a
# clean tag checkout, which is the default; `make docker-build` passes the
# real state for local builds.
ARG VERSION=v0.0.1
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG GIT_TREE_STATE=clean

# Build
# GOARCH comes from TARGETARCH, which BuildKit sets from --platform (or the
# host's platform when none is given), so the binary always matches the image.
RUN --mount=type=cache,id=gomod,target=/go/pkg,sharing=shared \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build,sharing=shared \
    set -eu; \
    V="${VERSION#v}"; \
    MAJOR="${V%%.*}"; REST="${V#*.}"; \
    MINOR="${REST%%.*}"; PATCH="${REST#*.}"; PATCH="${PATCH%%-*}"; \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/devzero-inc/zxporter/internal/version.major=${MAJOR} \
    -X github.com/devzero-inc/zxporter/internal/version.minor=${MINOR} \
    -X github.com/devzero-inc/zxporter/internal/version.patch=${PATCH} \
    -X github.com/devzero-inc/zxporter/internal/version.gitCommit=${COMMIT} \
    -X github.com/devzero-inc/zxporter/internal/version.gitTreeState=${GIT_TREE_STATE} \
    -X github.com/devzero-inc/zxporter/internal/version.buildDate=${BUILD_DATE}" \
    -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS production
RUN apk add --no-cache bash kubectl

FROM production
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

COPY ./entrypoint.sh /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh", "/manager"]

# OCI metadata last: per-release values must not invalidate the layers above.
ARG VERSION=v0.0.1
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="zxporter" \
      org.opencontainers.image.source="https://github.com/devzero-inc/services" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
