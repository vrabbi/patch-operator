# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/vrabbi/patch-operator:latest
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest.
ENVTEST_K8S_VERSION = 1.34.1

# Get the currently used golang install path
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRDs, RBAC and webhook manifests.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

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
# -coverpkg attributes the integration suite's coverage to the packages it exercises. Without it
# the controllers read as barely covered, because the tests that drive them live in another package.
test: manifests generate fmt vet setup-envtest ## Run unit and integration (envtest) tests.
	KUBEBUILDER_ASSETS="$(shell $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test $$(go list ./... | grep -v /test/e2e) \
			-coverpkg=./api/...,./internal/... -coverprofile cover.out -timeout 20m

.PHONY: test-unit
test-unit: fmt vet ## Run unit tests only (no envtest, no API server needed).
	go test ./api/... ./internal/render/... ./internal/scope/... ./internal/webhook/... -coverprofile cover-unit.out

.PHONY: test-e2e
test-e2e: ## Run e2e tests. Requires a running Kind cluster (see .github/workflows/e2e.yaml).
	go test ./test/e2e/ -v -ginkgo.v -timeout 30m

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run --timeout 10m

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint with --fix.
	$(GOLANGCI_LINT) run --fix --timeout 10m

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run the controller against the configured cluster.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build the container image.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push the container image.
	$(CONTAINER_TOOL) push ${IMG}

##@ Deployment

.PHONY: install
install: manifests kustomize ## Install CRDs into the cluster.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the cluster.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=true -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy the controller to the cluster.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy the controller from the cluster.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found=true -f -

.PHONY: deploy-namespaced-only
deploy-namespaced-only: manifests kustomize ## Deploy without the cluster-scoped CRDs or cluster write RBAC.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build $(NAMESPACED_ONLY_FLAGS) config/overlays/namespaced-only | kubectl apply -f -

# Selecting a subset of the generated CRDs means referencing files above the overlay's directory,
# which kustomize permits only with this flag.
NAMESPACED_ONLY_FLAGS ?= --load-restrictor LoadRestrictionsNone

##@ Docs

.PHONY: docs-serve
docs-serve: ## Serve the docs site locally.
	pip install -q -r docs/requirements.txt && mkdocs serve

.PHONY: docs-build
docs-build: ## Build the docs site (strict).
	pip install -q -r docs/requirements.txt && mkdocs build --strict

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
KUSTOMIZE ?= $(LOCALBIN)/kustomize
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

# The toolchain the module targets, read from go.mod so there is one source of truth.
GO_TOOLCHAIN ?= go$(shell sed -n 's/^go //p' go.mod)

CONTROLLER_TOOLS_VERSION ?= v0.22.0
KUSTOMIZE_VERSION ?= v5.7.1
ENVTEST_VERSION ?= release-0.25
GOLANGCI_LINT_VERSION ?= v2.6.1

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: kustomize
kustomize: $(KUSTOMIZE)
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: setup-envtest
setup-envtest: $(SETUP_ENVTEST)
$(SETUP_ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(SETUP_ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
# Built with the same toolchain the module targets, not via the shared go-install-tool macro.
#
# golangci-lint refuses to analyse a module whose go directive is newer than the Go version
# golangci-lint itself was compiled with, and a prebuilt release binary carries whatever was
# current when it shipped. Forcing GOTOOLCHAIN here keeps local and CI on the same footing instead
# of failing in only one of them.
$(GOLANGCI_LINT): $(LOCALBIN)
	@[ -f "$(GOLANGCI_LINT)-$(GOLANGCI_LINT_VERSION)" ] || { \
	set -e ;\
	package=github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) ;\
	echo "Downloading $${package} with $(GO_TOOLCHAIN)" ;\
	rm -f $(GOLANGCI_LINT) || true ;\
	GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(LOCALBIN) go install $${package} ;\
	mv $(GOLANGCI_LINT) $(GOLANGCI_LINT)-$(GOLANGCI_LINT_VERSION) ;\
	}
	@ln -sf $(GOLANGCI_LINT)-$(GOLANGCI_LINT_VERSION) $(GOLANGCI_LINT)

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
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

CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs
CRD_REF_DOCS_VERSION ?= v0.1.0

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS)
$(CRD_REF_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

.PHONY: api-docs
api-docs: crd-ref-docs ## Generate the API reference from the Go types.
	$(CRD_REF_DOCS) \
		--source-path=./api \
		--config=hack/crd-ref-docs.yaml \
		--renderer=markdown \
		--output-path=docs/reference/api-generated.md
