# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 0.0.1

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "candidate,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=candidate,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="candidate,fast,stable")
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# devzero.io/zxporter-bundle:$VERSION and devzero.io/zxporter-catalog:$VERSION.
IMAGE_TAG_BASE ?= devzero.io/zxporter

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)

# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# Set the Operator SDK version to use. By default, what is installed on the system is used.
# This is useful for CI or a project to utilize a specific version of the operator-sdk toolkit.
OPERATOR_SDK_VERSION ?= v1.39.1
# Image URLs to use all building/pushing image targets
IMG ?= ttl.sh/zxporter:latest
# Testserver image URL
TESTSERVER_IMG ?= ttl.sh/zxporter-testserver:latest
# Stress test image URL
STRESS_IMG ?= ttl.sh/zxporter-stress:latest
# DAKR URL to use for deployment
DAKR_URL ?= https://api.devzero.io/dakr
# PROMETHEUS URL for metrics collection
PROMETHEUS_URL ?= http://prometheus-dz-prometheus-server.$(DEVZERO_MONITORING_NAMESPACE).svc.cluster.local:80
# TARGET_NAMESPACES for limiting collection to specific namespaces (comma-separated)
TARGET_NAMESPACES ?= 
# COLLECTION_FILE is used to control the collectionpolicies.
COLLECTION_FILE ?= env_configmap.yaml
# ENV_CONFIGMAP_FILE is used to control the zxporter-manager deployment.
ENV_CONFIGMAP_FILE ?= config/manager/env_configmap.yaml

# Monitoring resources
PROMETHEUS_CHART_VERSION ?= 25.8.0
DEVZERO_MONITORING_NAMESPACE ?= devzero-zxporter
NODE_EXPORTER_CHART_VERSION ?= 4.24.0
METRICS_SERVER_CHART_VERSION ?= 3.12.0

# DIST_INSTALL_BUNDLE is the final complete manifest
DIST_DIR ?= dist
DIST_INSTALL_BUNDLE ?= $(DIST_DIR)/install.yaml
DIST_ZXPORTER_BUNDLE ?= $(DIST_DIR)/zxporter.yaml
DIST_PROMETHEUS_BUNDLE ?= $(DIST_DIR)/prometheus.yaml
DIST_NODE_EXPORTER_BUNDLE ?= $(DIST_DIR)/node-exporter.yaml
METRICS_SERVER ?= $(DIST_DIR)/metrics-server.yaml

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.31.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: test-collectors
test-collectors:
	go test ./internal/collector/... -v

# Utilize Kind or modify the e2e tests to load the image locally, enabling compatibility with other vendors.
.PHONY: test-e2e  # Run the e2e tests against a Kind k8s instance that is spun up.
test-e2e:
	go test ./test/e2e/ -v -ginkgo.v

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

GITVERSION ?= $(shell git describe --tags 2>/dev/null || echo "v0.0.0-$(shell git rev-parse --short HEAD)")

ifeq ($(shell echo $(GITVERSION) | grep -E "^v[0-9]+\.[0-9]+\.[0-9]+"),)
  MAJOR := 0
  MINOR := 0
  PATCH := 1
else
  MAJOR := $(shell echo $(GITVERSION) | sed -E 's/^v([0-9]+)\..*/\1/')
  MINOR := $(shell echo $(GITVERSION) | sed -E 's/^v[0-9]+\.([0-9]+)\..*/\1/')
  PATCH := $(shell echo $(GITVERSION) | sed -E 's/^v[0-9]+\.[0-9]+\.(.*)/\1/')
endif

GITVERSION ?= $(shell git describe --tags 2>/dev/null || echo "v0.0.0-$(shell git rev-parse --short HEAD)")
COMMIT_HASH ?= $(shell git rev-parse --short HEAD 2>/dev/null)
GIT_TREE_STATE ?= $(if $(shell git status --porcelain),dirty,clean)
BUILD_DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

GO_VERSION = $(shell go version | awk '{print $$3}')

LDFLAGS += -X github.com/devzero-inc/zxporter/internal/version.Get=*github.com/devzero-inc/zxporter/internal/version.Info
LDFLAGS += -X github.com/devzero-inc/zxporter/internal/version.Get.Major=$(MAJOR)
LDFLAGS += -X github.com/devzero-inc/zxporter/internal/version.Get.Minor=$(MINOR)
LDFLAGS += -X github.com/devzero-inc/zxporter/internal/version.Get.Patch=$(PATCH)
LDFLAGS += -X github.com/devzero-inc/zxporter/internal/version.Get.GitCommit=$(COMMIT_HASH)
LDFLAGS += -X github.com/devzero-inc/zxporter/internal/version.Get.GitTreeState=$(GIT_TREE_STATE)
LDFLAGS += -X github.com/devzero-inc/zxporter/internal/version.Get.BuildDate=$(BUILD_DATE)
LDFLAGS += -X github.com/devzero-inc/zxporter/internal/version.Get.GoVersion=$(GO_VERSION)

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -ldflags "$(LDFLAGS)" -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: helm ## Build docker image with the manager.
	@echo "[INFO] Adding Metrics Server repo"
	@$(HELM) repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ >> /dev/null || true
	@echo "[INFO] Fetching Metrics Server repo data"
	@$(HELM) repo update metrics-server >> /dev/null

	@echo "[INFO] Generate Metrics Server manifest"
	@$(HELM) template metrics-server metrics-server/metrics-server \
		--version $(METRICS_SERVER_CHART_VERSION) \
		--namespace devzero-zxporter \
		--set args="{--kubelet-insecure-tls}" \
		--set nameOverride="dz-metrics-server" \
		--set fullnameOverride="dz-metrics-server" \
		> $(METRICS_SERVER)

	@echo "[INFO] For debug -> $(GO_VERSION), major  $(MAJOR), minor  $(MINOR), patch  $(PATCH)"
	$(CONTAINER_TOOL) build --load \
			--build-arg MAJOR=$(MAJOR) \
			--build-arg MINOR=$(MINOR) \
			--build-arg PATCH=$(PATCH) \
			--build-arg COMMIT_HASH=$(COMMIT_HASH) \
			--build-arg GIT_TREE_STATE=$(GIT_TREE_STATE) \
			--build-arg BUILD_DATE=$(BUILD_DATE) \
			--build-arg GO_VERSION="$(GO_VERSION)" \
			-t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: testserver-docker-build
testserver-docker-build: ## Build docker image for the testserver.
	$(CONTAINER_TOOL) build -t ${TESTSERVER_IMG} -f Dockerfile.testserver .

.PHONY: testserver-docker-push
testserver-docker-push: ## Push docker image for the testserver.
	$(CONTAINER_TOOL) push ${TESTSERVER_IMG}

.PHONY: stress-docker-build
stress-docker-build: ## Build docker image for the stress test.
	$(CONTAINER_TOOL) build -t ${STRESS_IMG} -f Dockerfile.stress .

.PHONY: stress-docker-push
stress-docker-push: ## Push docker image for the stress test.
	$(CONTAINER_TOOL) push ${STRESS_IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name zxporter-builder
	$(CONTAINER_TOOL) buildx use zxporter-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm zxporter-builder
	rm Dockerfile.cross

.PHONY: generate-monitoring-manifests
generate-monitoring-manifests: helm ## Generate monitoring manifests for Prometheus and Node Exporter.
	@echo "[INFO] Adding Prometheus repo"
	@$(HELM) repo add prometheus-community https://prometheus-community.github.io/helm-charts >> /dev/null || true
	@echo "[INFO] Fetching prometheus repo data"
	@$(HELM) repo update prometheus-community >> /dev/null

	@echo "[INFO] Generate prometheus manifest"
	@$(HELM) template prometheus prometheus-community/prometheus \
		--version $(PROMETHEUS_CHART_VERSION) \
		--namespace $(DEVZERO_MONITORING_NAMESPACE) \
		--create-namespace \
		--values config/prometheus/hack.prometheus.values.yaml \
		> $(DIST_PROMETHEUS_BUNDLE)

	@echo "[INFO] Generate Node Exporter manifest"
	@$(HELM) template node-exporter prometheus-community/prometheus-node-exporter \
		--version $(NODE_EXPORTER_CHART_VERSION) \
		--namespace $(DEVZERO_MONITORING_NAMESPACE) \
		--create-namespace \
		--values config/prometheus/hack.node-exporter.values.yaml \
		> $(DIST_NODE_EXPORTER_BUNDLE)


.PHONY: build-installer
build-installer: manifests generate kustomize yq ## Generate a consolidated YAML with deployment.
	@mkdir -p $(DIST_DIR)

	@echo "[INFO] Generating manifests for monitoring components..."
	@$(MAKE) generate-monitoring-manifests
	@echo "[INFO] Monitoring manifests generated."

	@echo "[INFO] Generating installer bundle..."
	@echo "## ATTN KUBERNETES ADMINS! Read this..." > $(DIST_INSTALL_BUNDLE)
	@echo "#  If prometheus-server is already installed, and you want to use that version," >> $(DIST_INSTALL_BUNDLE)
	@echo "#  comment out the section from \"START PROM SERVER\" to \"END PROM SERVER\" and update the \"prometheusURL\" variable." >> $(DIST_INSTALL_BUNDLE)
	@echo -e "#" >> $(DIST_INSTALL_BUNDLE)
	@echo "#  If prometheus-node-exporter is already installed, and you want to use that version," >> $(DIST_INSTALL_BUNDLE)
	@echo "#  comment out the section from \"START PROM NODE EXPORTER\" to \"END PROM NODE EXPORTER\"" >> $(DIST_INSTALL_BUNDLE)
	@echo -e "# \n" >> $(DIST_INSTALL_BUNDLE)

	@echo "[INFO] Adding namespace to the main installer"
	@echo "apiVersion: v1" >> $(DIST_INSTALL_BUNDLE)
	@echo "kind: Namespace" >> $(DIST_INSTALL_BUNDLE)
	@echo "metadata:" >> $(DIST_INSTALL_BUNDLE)
	@echo "  labels:" >> $(DIST_INSTALL_BUNDLE)
	@echo "    control-plane: controller-manager" >> $(DIST_INSTALL_BUNDLE)
	@echo "    app.kubernetes.io/name: $(DEVZERO_MONITORING_NAMESPACE)" >> $(DIST_INSTALL_BUNDLE)
	@echo "  name: $(DEVZERO_MONITORING_NAMESPACE)" >> $(DIST_INSTALL_BUNDLE)

	@echo "[INFO] Append prometheus-server to the main installer"
	@echo "# ----- START PROM SERVER -----" >> $(DIST_INSTALL_BUNDLE)
	@cat $(DIST_PROMETHEUS_BUNDLE) >> $(DIST_INSTALL_BUNDLE)
	@echo "# ----- END PROM SERVER -----" >> $(DIST_INSTALL_BUNDLE)

	@echo "[INFO] Append prometheus-node-exporter to the main installer"
	@echo "# ----- START PROM NODE EXPORTER -----" >> $(DIST_INSTALL_BUNDLE)
	@cat $(DIST_NODE_EXPORTER_BUNDLE) >> $(DIST_INSTALL_BUNDLE)
	@echo "# ----- END PROM NODE EXPORTER -----" >> $(DIST_INSTALL_BUNDLE)

#	@echo "[INFO] Append Metrics Server to the main installer"
#	@echo "# ----- START METRICS SERVER -----" >> $(DIST_INSTALL_BUNDLE)
#	@cat $(METRICS_SERVER) >> $(DIST_INSTALL_BUNDLE)
#	@echo "# ----- END METRICS SERVER -----" >> $(DIST_INSTALL_BUNDLE)
	@echo "---" >> $(DIST_INSTALL_BUNDLE)
	
	@echo "[INFO] Append zxporter-manager to the installer bundle"
	@cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}

	@echo "[INFO] Replacing env variables in configmap"
	@$(YQ) e '.data.DAKR_URL = "$(DAKR_URL)"' -i $(ENV_CONFIGMAP_FILE)
	@$(YQ) e '.data.PROMETHEUS_URL = "$(PROMETHEUS_URL)"' -i $(ENV_CONFIGMAP_FILE)
	@$(YQ) e '.data.TARGET_NAMESPACES = "$(TARGET_NAMESPACES)"' -i $(ENV_CONFIGMAP_FILE)

	@$(KUSTOMIZE) build config/default > $(DIST_ZXPORTER_BUNDLE)
	@cat $(DIST_ZXPORTER_BUNDLE) >> $(DIST_INSTALL_BUNDLE)

.PHONY: build-env-configmap
build-env-configmap: DIST_INSTALL_BUNDLE=$(DIST_DIR)/env_configmap.yaml
build-env-configmap: yq
build-env-configmap:
	echo "" > $(DIST_INSTALL_BUNDLE)
	# Copy and patch environment config
	sed "s|\$$(DAKR_URL)|$(DAKR_URL)|g" $(ENV_CONFIGMAP_FILE) > temp.yaml && mv temp.yaml $(ENV_CONFIGMAP_FILE)
	sed "s|\$$(PROMETHEUS_URL)|$(PROMETHEUS_URL)|g" $(ENV_CONFIGMAP_FILE) > temp.yaml && mv temp.yaml $(ENV_CONFIGMAP_FILE)
	sed "s|\$$(TARGET_NAMESPACES)|$(TARGET_NAMESPACES)|g" $(ENV_CONFIGMAP_FILE) > temp.yaml && mv temp.yaml $(ENV_CONFIGMAP_FILE)
	$(KUSTOMIZE) build config/default | \
	yq eval 'select(.kind == "ConfigMap" and .metadata.name == "devzero-zxporter-env-config")' - >> $(DIST_INSTALL_BUNDLE)

.PHONY: build-chart
build-chart: build-installer ## Generate a consolidated helm chart from the installer manifest.
	@echo "[INFO] Generating helm chart from the installer bundle"
	@cat $(DIST_INSTALL_BUNDLE) | helmify chart
	@echo "[INFO] Helm chart generated in the 'chart' directory"

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: build-installer ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cat $(DIST_INSTALL_BUNDLE) | $(KUBECTL) apply -f -

.PHONY: local-deploy
local-deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: deploy-env-configmap
deploy-env-configmap: DIST_INSTALL_BUNDLE=$(DIST_DIR)/env_configmap.yaml
deploy-env-configmap: build-env-configmap
	cat $(DIST_INSTALL_BUNDLE) | $(KUBECTL) apply -f -

.PHONY: undeploy-monitoring
undeploy-monitoring: ## Undeploy monitoring components.
	$(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f $(DIST_NODE_EXPORTER_BUNDLE) || true
	$(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f $(DIST_PROMETHEUS_BUNDLE) || true

.PHONY: undeploy
undeploy: kustomize undeploy-monitoring ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	@echo "[BIN] Creating $(LOCALBIN) directory"
	@mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
HELM ?= $(LOCALBIN)/helm
# to download: https://github.com/mikefarah/yq?tab=readme-ov-file#install
YQ ?= $(LOCALBIN)/yq
# to download: `brew install arttor/tap/helmify`
HELMIFY ?= helmify

## Tool Versions
KUSTOMIZE_VERSION ?= v5.4.3
CONTROLLER_TOOLS_VERSION ?= v0.16.1
ENVTEST_VERSION ?= release-0.19
GOLANGCI_LINT_VERSION ?= v1.59.1
HELM_VERSION ?= v3.14.2
YQ_VERSION ?= v4.40.5

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	@if [ ! -f $(HELM) ]; then \
		OS=$$(go env GOOS) ;\
		ARCH=$$(go env GOARCH) ;\
		echo "[BIN] Downloading helm $(HELM_VERSION) $$VERSION for $$OS/$$ARCH..." ;\
		curl -sSLo helm.tar.gz https://get.helm.sh/helm-$(HELM_VERSION)-$$OS-$$ARCH.tar.gz ;\
		tar -zxvf helm.tar.gz -C $(LOCALBIN) --strip-components=1 $$OS-$$ARCH/helm ;\
		rm -f helm.tar.gz ;\
	fi

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

.PHONY: yq
yq: $(YQ) ## Download yq locally if necessary.
$(YQ): $(LOCALBIN)
	@if [ ! -f $(YQ) ]; then \
		OS=$(shell go env GOOS) ;\
		ARCH=$(shell go env GOARCH) ;\
		echo "[BIN]  Downloading yq $(YQ_VERSION) for $$OS/$$ARCH..." ;\
		curl -sSLo $(YQ) https://github.com/mikefarah/yq/releases/download/$(YQ_VERSION)/yq_$${OS}_$${ARCH} ;\
		chmod +x $(YQ) ;\
	fi

.PHONY: operator-sdk
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
	echo "[BIN] Downloading operator-sdk $(OPERATOR_SDK_VERSION) for $$OS/$$ARCH..." ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate bundle manifests and metadata, then validate generated files.
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	sed "s|\$$(DAKR_URL)|$(DAKR_URL)|g" config/manager/env_configmap.yaml > temp.yaml && mv temp.yaml config/manager/env_configmap.yaml
	sed "s|\$$(TARGET_NAMESPACES)|$(TARGET_NAMESPACES)|g" config/manager/env_configmap.yaml > temp.yaml && mv temp.yaml config/manager/env_configmap.yaml
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	docker build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) docker-push IMG=$(BUNDLE_IMG)

.PHONY: opm
OPM = $(LOCALBIN)/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.23.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

# A comma-separated list of bundle images (e.g. make catalog-build BUNDLE_IMGS=example.com/operator-bundle:v0.1.0,example.com/operator-bundle:v0.2.0).
# These images MUST exist in a registry and be pull-able.
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

# Build a catalog image by adding bundle images to an empty catalog using the operator package manager tool, 'opm'.
# This recipe invokes 'opm' in 'semver' bundle add mode. For more information on add modes, see:
# https://github.com/operator-framework/community-operators/blob/7f1438c/docs/packaging-operator.md#updating-your-existing-operator
.PHONY: catalog-build
catalog-build: opm ## Build a catalog image.
	$(OPM) index add --container-tool docker --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT)

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	$(MAKE) docker-push IMG=$(CATALOG_IMG)

# DAKR_* files are related to generating a gRPC client to send resource metadata to Dakr.
DAKR_DIR ?= /Users/kevinshi/services/dakr
DAKR_BUF_GEN_FILE ?= buf.gen.yaml
DAKR_BUF_GEN ?= $(DAKR_DIR)/$(DAKR_BUF_GEN_FILE)
DAKR_METRICS_COLLECTOR_PROTO_FILE ?= metrics_collector.proto
DAKR_METRICS_COLLECTOR_PROTO ?= $(DAKR_DIR)/proto/api/v1/$(DAKR_METRICS_COLLECTOR_PROTO_FILE)

# BUF_VERSION and BUF_BINARY_NAME are to generate a Dakr protobuf/gRPC client.
BUF_VERSION := 1.31.0
BUF_BINARY_NAME := buf

# Install buf
.PHONY: install-buf
install-buf: ## Install buf if not already installed
	@if ! command -v $(BUF_BINARY_NAME) >/dev/null 2>&1; then \
		echo "Installing $(BUF_BINARY_NAME)..."; \
		curl -sSL "https://github.com/bufbuild/buf/releases/download/v${BUF_VERSION}/${BUF_BINARY_NAME}-$(shell uname -s)-$(shell uname -m)" -o "/usr/local/bin/${BUF_BINARY_NAME}"; \
		chmod +x "/usr/local/bin/${BUF_BINARY_NAME}"; \
	else \
		echo "$(BUF_BINARY_NAME) is already installed."; \
	fi

# (for local dev) Get metadata to generate a Dakr client. 
.PHONY: generate-proto
generate-proto: install-buf ## Fetch latest Dakr protobuf
	@PROTO_DIR="$(PWD)/proto"; \
	echo "Cleaning $$PROTO_DIR..."; \
	rm -rf "$$PROTO_DIR"/*; \
	rm -rf "$(PWD)/gen"; \
	mkdir -p "$$PROTO_DIR"; \
	cp "$(DAKR_BUF_GEN)" "$$PROTO_DIR/"; \
	cp "$(DAKR_METRICS_COLLECTOR_PROTO)" "$$PROTO_DIR/"; \
	find "$$PROTO_DIR" -type f -name "*.yaml" -exec perl -pi -e 's|github.com/devzero-inc/services/dakr/gen|github.com/devzero-inc/zxporter/gen|g' {} +; \
	buf build "$(DAKR_DIR)" --path "$(DAKR_METRICS_COLLECTOR_PROTO)" -o "$$PROTO_DIR"/dakr_proto_descriptor.bin; \
	buf generate --template "$$PROTO_DIR"/"$(DAKR_BUF_GEN_FILE)" --include-imports "$$PROTO_DIR"/dakr_proto_descriptor.bin;
