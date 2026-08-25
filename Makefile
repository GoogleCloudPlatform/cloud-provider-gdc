# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

export GOFLAGS ?= -mod=mod

REGISTRY                          := europe-docker.pkg.dev/gardener-project/public
EXECUTABLE                        := gdch-cloud-controller-manager
REPO_ROOT                         := $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))
IMAGE_REPOSITORY                  := $(REGISTRY)/cloud-controller-manager-gdch
VERSION                           ?= v0.1.0-dev
IMAGE_TAG                         ?= $(VERSION)

#########################################
# Tools                                 #
#########################################

TOOLS_DIR     := .tools
TOOLS_BIN_DIR := $(TOOLS_DIR)/bin
export PATH   := $(abspath $(TOOLS_BIN_DIR)):$(PATH)

GOIMPORTS      := $(TOOLS_BIN_DIR)/goimports
GOLANGCI_LINT  := $(TOOLS_BIN_DIR)/golangci-lint
GINKGO         := $(TOOLS_BIN_DIR)/ginkgo

GOLANGCI_LINT_VERSION ?= v2.1.6
GINKGO_VERSION        ?= v2.28.1

$(GOIMPORTS):
	@mkdir -p $(TOOLS_BIN_DIR)
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install golang.org/x/tools/cmd/goimports@latest

$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS_BIN_DIR)
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) CGO_ENABLED=1 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GINKGO):
	@mkdir -p $(TOOLS_BIN_DIR)
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install github.com/onsi/ginkgo/v2/ginkgo@$(GINKGO_VERSION)

.PHONY: all
all: build-local test

.PHONY: tidy
tidy:
	@go mod tidy

.PHONY: clean
clean:
	@rm -rf $(EXECUTABLE) bin/ $(TOOLS_DIR)

.PHONY: check
check: format $(GOLANGCI_LINT)
	@echo "Running golangci-lint..."
	@$(GOLANGCI_LINT) run --config=./.golangci.yaml ./cmd/... ./pkg/... ./gdc/...
	@echo "Running go vet..."
	@go vet ./cmd/... ./pkg/... ./gdc/...

.PHONY: format
format: $(GOIMPORTS)
	@$(GOIMPORTS) -l -w -local $(shell go list -m) ./cmd ./pkg ./gdc

.PHONY: build-local
build-local:
	@CGO_ENABLED=1 go build -o $(EXECUTABLE) \
	    -race \
	    -ldflags "-X main.Version=$(VERSION)-$(shell git rev-parse HEAD)" \
	    ./cmd/gdch-cloud-controller-manager

.PHONY: release
release: $(EXECUTABLE)

# LDFLAGS_RELEASE:
#   -s -w  strip symbol table and DWARF (smaller binary; no debuggability loss vs -w alone)
LDFLAGS_RELEASE ?= -s -w

# GOFLAGS_RELEASE:
#   -trimpath      remove absolute paths from binaries (reproducible builds).
#   -buildvcs=false suppress VCS stamping (source tree in the Docker builder has no .git).
GOFLAGS_RELEASE ?= -trimpath -buildvcs=false

$(EXECUTABLE): FORCE
	@CGO_ENABLED=0 go build $(GOFLAGS_RELEASE) -o $@ -ldflags '$(LDFLAGS_RELEASE) -X main.Version=$(VERSION)' ./cmd/gdch-cloud-controller-manager

.PHONY: FORCE
FORCE:

.PHONY: test unittests
test: unittests
unittests: $(GINKGO)
	@go test -race -timeout=3m ./pkg/... ./gdc/... ./cmd/...

.PHONY: docker-images
docker-images:
	@docker build -t $(IMAGE_REPOSITORY):$(IMAGE_TAG) -f Dockerfile --target gdch-cloud-controller-manager .

.PHONY: help
help: ## Display available targets
	@echo "Cloud Provider GDCH Build System"
	@echo "================================"
	@echo "Available make targets:"
	@echo "  make format        - Formats all Go source files with goimports"
	@echo "  make check         - Runs code linters (golangci-lint, go vet)"
	@echo "  make test          - Runs unit test suite across all packages"
	@echo "  make unittests     - Alias for test"
	@echo "  make build-local   - Builds binaries locally in current environment"
	@echo "  make release       - Builds cross-compiled release binaries"
	@echo "  make docker-images - Builds multi-stage Docker images for controller manager"
	@echo "  make tidy          - Runs go mod tidy"
	@echo "  make clean         - Cleans built binaries and tools cache"
