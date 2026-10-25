# SchemAxis Local Connector: build targets

BINARY_NAME=db-agent
# A build from here reports "dev" unless it is given a version:
#   make build VERSION=1.2.3
VERSION?=dev
BUILD_DIR=bin
# Read from go.mod, so that the module path is written in one place only.
MODULE:=$(shell go list -m)
# -trimpath keeps this machine's directory layout out of the binary, as the
# release builds do.
BUILD_FLAGS=-trimpath -ldflags "-s -w -X $(MODULE)/internal/branding.Version=$(VERSION)"

.PHONY: all build build-windows build-linux build-darwin build-all clean test install

all: build

# Build for the current platform
build:
	go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/agent/

# Cross-compile targets
build-windows:
	GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)_windows_amd64.exe ./cmd/agent/

build-linux:
	GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)_linux_amd64 ./cmd/agent/

build-darwin:
	GOOS=darwin GOARCH=arm64 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)_darwin_arm64 ./cmd/agent/

build-all: build-windows build-linux build-darwin

# Run tests
test:
	go test ./...

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)

# Install where go install would: GOBIN, or else the bin directory under
# GOPATH, which go env reports even when the variable is not set.
INSTALL_DIR:=$(or $(shell go env GOBIN),$(shell go env GOPATH)/bin)
install: build
	mkdir -p "$(INSTALL_DIR)"
	cp $(BUILD_DIR)/$(BINARY_NAME) "$(INSTALL_DIR)/"
