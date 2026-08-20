# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/beacon-operator/beacon:latest
# Bundle image
BUNDLE_IMG ?= ghcr.io/beacon-operator/beacon-bundle:latest
VERSION ?= 0.1.24

# OLM channel configuration (baked into the bundle metadata).
CHANNELS ?= stable
DEFAULT_CHANNEL ?= stable
BUNDLE_CHANNELS := --channels=$(CHANNELS)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)
# OpenShift version compatibility range advertised to OperatorHub. operator-sdk
# does not manage this annotation, so `make bundle` re-stamps it after generate.
OPENSHIFT_VERSIONS ?= v4.14-v4.21

# OLM upgrade graph: the CSV this release replaces. Leave empty for the first
# release in a channel. Example: make bundle VERSION=0.1.21 REPLACES=beacon.v0.1.20
REPLACES ?=

# Get the currently used golang install path
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

CONTAINER_TOOL ?= docker
LOCALBIN ?= $(shell pwd)/bin

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRDs, RBAC, etc.
	# No `webhook` generator: Beacon has no admission/conversion webhooks (it would
	# otherwise emit an empty config/crd/bases/_.yaml).
	$(CONTROLLER_GEN) rbac:roleName=beacon-manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

##@ Build

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: fmt vet ## Run a controller from your host against the configured cluster.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build container image (single-arch, host platform).
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push container image.
	$(CONTAINER_TOOL) push ${IMG}

# Platforms for the multi-arch manager image. amd64 + arm64 covers x86 and
# ARM-based (e.g. AWS Graviton, bare-metal ARM) spoke clusters.
PLATFORMS ?= linux/amd64,linux/arm64
.PHONY: docker-buildx
docker-buildx: ## Build and push a multi-arch manager image (buildx).
	# The Dockerfile already honours TARGETOS/TARGETARCH for cross-compilation.
	- $(CONTAINER_TOOL) buildx create --name beacon-builder --use 2>/dev/null || $(CONTAINER_TOOL) buildx use beacon-builder
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) \
	  --build-arg VERSION=$(VERSION) -t ${IMG} .
	- $(CONTAINER_TOOL) buildx rm beacon-builder 2>/dev/null || true

##@ Deployment

.PHONY: install
install: manifests kustomize ## Install CRDs into the cluster.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the cluster.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the cluster.
	cd config/manager && $(KUSTOMIZE) edit set image ghcr.io/beacon-operator/beacon=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the cluster.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found -f -

##@ OLM Bundle

# The bundle (bundle/manifests, bundle/metadata) is fully regenerated from
# config/ — the CRD comes from config/crd/bases, RBAC/Deployment from
# config/rbac + config/manager, and the CSV metadata from
# config/manifests/bases/beacon.clusterserviceversion.yaml. Do not hand-edit
# bundle/manifests; edit the config/ sources and re-run `make bundle`.
.PHONY: bundle
bundle: manifests kustomize ## Generate bundle manifests and metadata.
	operator-sdk generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image ghcr.io/beacon-operator/beacon=${IMG}
	$(KUSTOMIZE) build config/manifests | operator-sdk generate bundle -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)
	# operator-sdk drops the OpenShift version-compatibility annotation on every
	# regenerate; re-stamp it into the bundle metadata and Dockerfile.
	$(MAKE) stamp-openshift-versions
	# Stamp the OLM upgrade-graph edge (spec.replaces) when REPLACES is set.
	@if [ -n "$(REPLACES)" ]; then \
	  grep -q '^  replaces:' bundle/manifests/beacon.clusterserviceversion.yaml \
	    && sed -i.bak 's/^  replaces:.*/  replaces: $(REPLACES)/' bundle/manifests/beacon.clusterserviceversion.yaml \
	    || sed -i.bak 's/^  version: $(VERSION)/  version: $(VERSION)\n  replaces: $(REPLACES)/' bundle/manifests/beacon.clusterserviceversion.yaml; \
	  rm -f bundle/manifests/beacon.clusterserviceversion.yaml.bak; \
	fi
	operator-sdk bundle validate ./bundle

# CRD source of truth is config/crd/bases; `make bundle` copies it into the
# bundle, where operator-sdk adds boilerplate: a leading "---", a
# "creationTimestamp: null" metadata line, and a trailing empty "status:" block.
# This check diffs the two after stripping exactly that boilerplate, so genuine
# schema drift fails CI while the cosmetic additions are ignored. Uses only
# coreutils (sed/grep/diff) so it needs no Python/YAML deps in CI.
CRD_SRC := config/crd/bases/beacon.io_gatewayhealthpolicies.yaml
CRD_BUNDLE := bundle/manifests/beacon.io_gatewayhealthpolicies.yaml

# Normalizer: drop a leading document marker, the creationTimestamp line, and
# the operator-sdk trailing status stanza; collapse nothing else.
define _crd_normalize
sed -e '/^---$$/d' -e '/^  creationTimestamp: null$$/d' "$(1)" \
  | sed -e '/^status:$$/,$$d'
endef

.PHONY: verify-bundle-crd
verify-bundle-crd: ## Fail if the bundle CRD schema drifted from config/crd/bases.
	@diff -u <($(call _crd_normalize,$(CRD_SRC))) <($(call _crd_normalize,$(CRD_BUNDLE))) \
	  && echo "bundle CRD spec is in sync with config/crd/bases" \
	  || { echo "ERROR: bundle CRD drifted from $(CRD_SRC). Run 'make bundle'."; exit 1; }

.PHONY: stamp-openshift-versions
stamp-openshift-versions: ## Re-add com.redhat.openshift.versions to bundle metadata.
	@grep -q 'com.redhat.openshift.versions' bundle/metadata/annotations.yaml || { \
	  printf '\n  # OpenShift version compatibility.\n  com.redhat.openshift.versions: "%s"\n' '$(OPENSHIFT_VERSIONS)' >> bundle/metadata/annotations.yaml; }
	@grep -q 'com.redhat.openshift.versions' bundle.Dockerfile || { \
	  printf '\n# OpenShift version compatibility.\nLABEL com.redhat.openshift.versions="%s"\n' '$(OPENSHIFT_VERSIONS)' >> bundle.Dockerfile; }

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(CONTAINER_TOOL) push $(BUNDLE_IMG)

##@ Tooling

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
KUSTOMIZE ?= $(LOCALBIN)/kustomize
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

.PHONY: controller-gen
controller-gen: $(LOCALBIN) ## Install controller-gen.
	test -s $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.2

.PHONY: kustomize
kustomize: $(LOCALBIN) ## Install kustomize.
	test -s $(KUSTOMIZE) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@v5.4.2

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN) ## Install golangci-lint.
	test -s $(GOLANGCI_LINT) || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.59.1
