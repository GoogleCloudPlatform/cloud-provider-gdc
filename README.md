# cloud-provider-gdc

## Introduction

This repository implements the [cloud provider](https://github.com/kubernetes/cloud-provider) interface for Google Distributed Cloud (GDC).
It provides components for Kubernetes clusters running on GDC.

## Components

This repository contains the following components, located in `cmd/`:

*   **Cloud Controller Manager (`gdch-cloud-controller-manager`)**: The GDC [Cloud Controller Manager (CCM)](https://kubernetes.io/docs/concepts/architecture/cloud-controller/) is responsible for running cloud-provider-dependent controllers (e.g. node health, load balancing, etc.) for Kubernetes clusters running in GDC.

## Development & Build Workflow

This repository uses a standard Go toolchain and `Makefile` matching upstream Gardener build standards.

### Prerequisites
- Go 1.25 or higher
- Docker (for building container images)

### Common Make Targets

| Target | Description |
| :--- | :--- |
| `make format` | Formats all Go source files with `goimports` |
| `make check` | Runs code linters (`golangci-lint`, `go vet`) |
| `make unittests` | Runs unit test suite across all packages |
| `make build-local` | Builds binaries locally in current environment |
| `make release` | Builds cross-compiled release binaries |
| `make docker-images` | Builds multi-stage Docker images for controller manager |
| `make clean` | Cleans built binaries and test tools cache |

### Managing Dependencies

- **Add a new dependency**:
  ```bash
  go get <package-name>
  go mod tidy
  ```
- **Verify and download dependencies**:
  ```bash
  go mod download
  go mod verify
  ```
- **Format and check code before submitting**:
  ```bash
  make format
  make check
  make unittests
  ```

## License

`cloud-provider-gdc` is licensed under the Apache 2.0 license.
