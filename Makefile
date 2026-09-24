# The images are built by the Dagger module in .dagger/, which is their only
# definition (docs/adr/0005-images-built-by-dagger.md). IMAGE_REPO is the
# Operator's repository; the Exec Agent's is below it.
IMAGE_REPO ?= ghcr.io/phoban01/battery-operator
IMAGE_TAG ?= latest
# Image URL to use all building/pushing image targets
IMG ?= $(IMAGE_REPO):$(IMAGE_TAG)
EXEC_AGENT_IMG ?= $(IMAGE_REPO)/exec-agent:$(IMAGE_TAG)
# battery's poolmgrd image, the Operator's sidecar (config/manager; `make
# deploy` and `make build-installer` set it there). Its tag is
# the battery version go.mod pins, without the leading v, so the sidecar and
# the client's protos cannot drift apart.
BATTERY_VERSION ?= $(shell awk '$$1 == "github.com/liquidmetal-dev/battery" { print $$2 }' go.mod)
POOLMGRD_IMG ?= ghcr.io/liquidmetal-dev/poolmgrd:$(BATTERY_VERSION:v%=%)
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

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
# The duvet targets are described the other way, by a comment line of the form
# "## target: description" above the target, and the awk prints those too.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^## [a-zA-Z_0-9-]+: / { n = index($$0, ": "); printf "  \033[36m%-15s\033[0m  %s\n", substr($$0, 4, n - 4), substr($$0, n + 2) } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="$(CONTROLLER_GEN_PATHS)" output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="$(CONTROLLER_GEN_PATHS)"

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest kustomize ## Run tests.
	KUSTOMIZE="$(KUSTOMIZE)" KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# The e2e suite (test/e2e/README.md, ADR 0006): sigs.k8s.io/e2e-framework
# creates a kind cluster from test/e2e/kind-config.yaml, loads the images
# into it, installs cert-manager, deploys the Manifests through the overlay
# test/e2e/config, runs the features and deletes the cluster. test-e2e
# builds the three images with Dagger first, tagged E2E_IMAGE_TAG; the
# nodes pull poolmgrd, POOLMGRD_IMG, themselves.
#
# - E2E_KEEP_CLUSTER=true leaves the cluster in place, and a later run
#   reuses it; cleanup-test-e2e deletes it.
# - E2E_LOGS_DIR=<dir> exports the cluster's logs there at the end.
# - E2E_ARGS passes more flags to go test, for example
#   E2E_ARGS='-run TestCRDValidation'.
KIND_CLUSTER ?= battery-operator-e2e
E2E_IMAGE_TAG ?= e2e
FAKE_FLINTLOCKD_IMG ?= $(IMAGE_REPO)/fake-flintlockd:$(IMAGE_TAG)
E2E_ARGS ?=

.PHONY: docker-build-e2e
docker-build-e2e: IMAGE_TAG = $(E2E_IMAGE_TAG)
docker-build-e2e: docker-build ## Build the Operator, Exec Agent and fake flintlockd images for the e2e suite and load them into Docker.
	$(DAGGER) call fake-flintlockd-image export-image --name=${FAKE_FLINTLOCKD_IMG}

.PHONY: test-e2e
test-e2e: IMAGE_TAG = $(E2E_IMAGE_TAG)
test-e2e: manifests kustomize docker-build-e2e test-e2e-only ## Build the images, then run the e2e suite on a kind cluster of its own.

.PHONY: test-e2e-only
test-e2e-only: IMAGE_TAG = $(E2E_IMAGE_TAG)
test-e2e-only: kustomize ## Run the e2e suite with images already built by docker-build-e2e.
	cd test/e2e && KIND_CLUSTER=$(KIND_CLUSTER) KUSTOMIZE="$(KUSTOMIZE)" KUBECTL="$(KUBECTL)" \
		IMG=$(IMG) EXEC_AGENT_IMG=$(EXEC_AGENT_IMG) FAKE_FLINTLOCKD_IMG=$(FAKE_FLINTLOCKD_IMG) POOLMGRD_IMG=$(POOLMGRD_IMG) \
		go test -tags=e2e -v -count=1 -timeout=40m . $(E2E_ARGS)

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Delete the kind cluster a run with E2E_KEEP_CLUSTER=true left.
	$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the manager and Exec Agent binaries.
	go build -o bin/manager cmd/main.go
	go build -o bin/exec-agent ./cmd/exec-agent

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# The image targets call the Dagger module in .dagger/. docker-build builds
# both images for the Dagger engine's platform and loads them into the local
# Docker, for example for kind. docker-push builds them for linux/amd64 and
# linux/arm64 and pushes them to IMAGE_REPO at IMAGE_TAG, with the
# credentials of `docker login`; CI publishes the same way.
.PHONY: docker-build
docker-build: ## Build the Operator and Exec Agent images with Dagger and load them into Docker as IMG and EXEC_AGENT_IMG.
	$(DAGGER) call operator-image export-image --name=${IMG}
	$(DAGGER) call exec-agent-image export-image --name=${EXEC_AGENT_IMG}

.PHONY: docker-push
docker-push: ## Build both images for linux/amd64 and linux/arm64 with Dagger and push them to IMAGE_REPO at IMAGE_TAG.
	$(DAGGER) call publish --repository=${IMAGE_REPO} --tags=${IMAGE_TAG}

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image ghcr.io/phoban01/battery-operator=${IMG} ghcr.io/liquidmetal-dev/poolmgrd=${POOLMGRD_IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image ghcr.io/phoban01/battery-operator=${IMG} ghcr.io/liquidmetal-dev/poolmgrd=${POOLMGRD_IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Requirements

DUVET ?= duvet

.PHONY: duvet duvet-ci duvet-open coverage-gate coverage-ratchet duvet-models duvet-models-write

## duvet: extract requirements, build the HTML/JSON reports and refresh the snapshot
# --ci false is explicit: duvet turns the snapshot check on by itself when CI
# is set in the environment, which would make this target reject every PR
# that adds a citation. The snapshot is checked only by duvet-ci, at
# milestones. The second report is the models' (.duvet/models.toml), and the
# target fails if README's list of modelled requirements is out of date.
duvet:
	rm -rf .duvet/requirements
	$(DUVET) report --ci false
	DUVET=$(DUVET) hack/duvet-models.sh --check

## duvet-ci: same as duvet, but fail if .duvet/snapshot.txt would change (milestones only)
duvet-ci:
	rm -rf .duvet/requirements
	$(DUVET) report --ci true

## duvet-open: build the report and open it in a browser
duvet-open: duvet
	xdg-open .duvet/reports/report.html 2>/dev/null || open .duvet/reports/report.html

## coverage-gate: fail unless every ID in IDS has an implementation and a test citation
coverage-gate:
	@if [ -z "$(IDS)" ]; then echo 'usage: make coverage-gate IDS="CL-001 CL-002"' >&2; exit 2; fi
	DUVET=$(DUVET) hack/duvet-coverage.sh $(IDS)

## coverage-ratchet: fail if a requirement cited in code and a test at the merge base of BASE (default origin/main) lost either
BASE ?= origin/main
coverage-ratchet:
	DUVET=$(DUVET) hack/duvet-coverage.sh --base $(BASE) $(IDS)

## duvet-models: list every requirement as implemented, tested and modelled
duvet-models: duvet
	SKIP_REPORT=1 hack/duvet-models.sh --status

## duvet-models-write: rewrite the list of modelled requirements in docs/requirements/README.md
duvet-models-write:
	DUVET=$(DUVET) hack/duvet-models.sh --write

##@ Models

QUINT ?= quint

.PHONY: quint

## quint: typecheck, test and simulate the Quint models in specs/quint, checking their invariants
quint:
	QUINT=$(QUINT) hack/quint.sh

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
DAGGER ?= dagger
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

# controller-gen reads the module's packages by import path. Given "./..." it
# walks into every go.mod below the checkout, including the module cache in
# devbox's project-local GOPATH (.gopath/), and fails there.
CONTROLLER_GEN_PATHS ?= $(shell go list -m)/...

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.12.2
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	@# Once the build has put controller-runtime in the module cache, go install
	@# looks for setup-envtest in that module rather than in its own, and fails.
	@# Downloading setup-envtest's module first avoids that.
	go mod download sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
