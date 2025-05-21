# Build the manager binary
FROM golang:1.24.1 AS builder
ARG TARGETOS
ARG TARGETARCH
ENV GOOS=${TARGETOS}
ENV GOARCH=${TARGETARCH}

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.sum ./

# COPY go.sum go.sum
# # cache deps before building and copying source so that we don't need to re-download as much
# # and so that source changes don't invalidate our downloaded layer
RUN go mod download


COPY . .

ARG MAJOR=0
ARG MINOR=0
ARG PATCH=1
ARG GITVERSION=unknown
ARG COMMIT_HASH=unknown
ARG GIT_TREE_STATE=unknown
ARG BUILD_DATE=unknown
ARG GO_VERSION=unknown

RUN echo "MAJOR: ${MAJOR}"
RUN echo "MINOR: ${MINOR}"
RUN echo "PATCH: ${PATCH}"
RUN echo "GITVERSION: ${GITVERSION}"
RUN echo "COMMIT_HASH: ${COMMIT_HASH}"
RUN echo "GIT_TREE_STATE: ${GIT_TREE_STATE}"
RUN echo "BUILD_DATE: ${BUILD_DATE}"
RUN echo "GO_VERSION: ${GO_VERSION}"
RUN echo "TARGETOS: ${TARGETOS:-linux}"
RUN echo "TARGETARCH: ${TARGETARCH}"

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/devzero-inc/zxporter/internal/version.major=${MAJOR} \
    -X github.com/devzero-inc/zxporter/internal/version.minor=${MINOR} \
    -X github.com/devzero-inc/zxporter/internal/version.patch=${PATCH} \
    -X github.com/devzero-inc/zxporter/internal/version.gitCommit=${COMMIT_HASH} \
    -X github.com/devzero-inc/zxporter/internal/version.gitTreeState=${GIT_TREE_STATE} \
    -X github.com/devzero-inc/zxporter/internal/version.buildDate=${BUILD_DATE} \
    -X github.com/devzero-inc/zxporter/internal/version.goVersion=${GO_VERSION}" \
    -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c AS production
RUN apk add --no-cache bash kubectl

FROM production
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

COPY ./dist/metrics-server.yaml /metrics-server.yaml
COPY ./entrypoint.sh /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh", "/manager"]
