# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/bmarlow/beacon:latest
# Bundle image
BUNDLE_IMG ?= ghcr.io/bmarlow/beacon-bundle:latest
VERSION ?= 0.1.0

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
	$(CONTROLLER_GEN) rbac:roleName=beacon-manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

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
docker-build: ## Build container image.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push container image.
	$(CONTAINER_TOOL) push ${IMG}

##@ Deployment

.PHONY: install
install: manifests kustomize ## Install CRDs into the cluster.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the cluster.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the cluster.
	cd config/manager && $(KUSTOMIZE) edit set image ghcr.io/bmarlow/beacon=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the cluster.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found -f -

##@ OLM Bundle

.PHONY: bundle
bundle: manifests kustomize ## Generate bundle manifests and metadata.
	operator-sdk generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image ghcr.io/bmarlow/beacon=${IMG}
	$(KUSTOMIZE) build config/manifests | operator-sdk generate bundle -q --overwrite --version $(VERSION)
	operator-sdk bundle validate ./bundle

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
	test -s $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.15.0

.PHONY: kustomize
kustomize: $(LOCALBIN) ## Install kustomize.
	test -s $(KUSTOMIZE) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@v5.4.2

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN) ## Install golangci-lint.
	test -s $(GOLANGCI_LINT) || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.59.1
