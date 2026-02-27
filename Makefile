SHELL := /bin/bash
.PHONY: oapi-generate generate-vmm-client generate-wire generate-all dev build build-linux test test-linux test-darwin install-tools gen-jwt download-ch-binaries download-ch-spec ensure-ch-binaries build-caddy-binaries build-caddy ensure-caddy-binaries  release-prep clean build-embedded

# Directory where local binaries will be installed
BIN_DIR ?= $(CURDIR)/bin

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

# Local binary paths
OAPI_CODEGEN ?= $(BIN_DIR)/oapi-codegen
OAPI_CODEGEN_VERSION ?= v2.5.1
AIR ?= $(BIN_DIR)/air
WIRE ?= $(BIN_DIR)/wire
XCADDY ?= $(BIN_DIR)/xcaddy

# Install oapi-codegen
$(OAPI_CODEGEN): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)

# Install air for hot reload
$(AIR): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/air-verse/air@latest

# Install wire for dependency injection
$(WIRE): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/google/wire/cmd/wire@latest

# Install xcaddy for building Caddy with plugins
$(XCADDY): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

install-tools: $(OAPI_CODEGEN) $(AIR) $(WIRE) $(XCADDY)

# Download Cloud Hypervisor binaries
download-ch-binaries:
	@echo "Downloading Cloud Hypervisor binaries..."
	@mkdir -p lib/vmm/binaries/cloud-hypervisor/v48.0/{x86_64,aarch64}
	@mkdir -p lib/vmm/binaries/cloud-hypervisor/v49.0/{x86_64,aarch64}
	@echo "Downloading v48.0..."
	@curl -L -o lib/vmm/binaries/cloud-hypervisor/v48.0/x86_64/cloud-hypervisor \
		https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v48.0/cloud-hypervisor-static
	@curl -L -o lib/vmm/binaries/cloud-hypervisor/v48.0/aarch64/cloud-hypervisor \
		https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v48.0/cloud-hypervisor-static-aarch64
	@echo "Downloading v49.0..."
	@curl -L -o lib/vmm/binaries/cloud-hypervisor/v49.0/x86_64/cloud-hypervisor \
		https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v49.0/cloud-hypervisor-static
	@curl -L -o lib/vmm/binaries/cloud-hypervisor/v49.0/aarch64/cloud-hypervisor \
		https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v49.0/cloud-hypervisor-static-aarch64
	@chmod +x lib/vmm/binaries/cloud-hypervisor/v*/*/cloud-hypervisor
	@echo "Binaries downloaded successfully"

# Caddy version and modules
CADDY_VERSION := v2.10.2
CADDY_DNS_MODULES := --with github.com/caddy-dns/cloudflare

# Build Caddy with DNS modules using xcaddy
# xcaddy builds Caddy from source with the specified modules
build-caddy-binaries: $(XCADDY)
	@echo "Building Caddy $(CADDY_VERSION) with DNS modules..."
	@mkdir -p lib/ingress/binaries/caddy/$(CADDY_VERSION)/x86_64
	@mkdir -p lib/ingress/binaries/caddy/$(CADDY_VERSION)/aarch64
	@echo "Building Caddy $(CADDY_VERSION) for x86_64..."
	GOOS=linux GOARCH=amd64 $(XCADDY) build $(CADDY_VERSION) \
		$(CADDY_DNS_MODULES) \
		--output lib/ingress/binaries/caddy/$(CADDY_VERSION)/x86_64/caddy
	@echo "Building Caddy $(CADDY_VERSION) for aarch64..."
	GOOS=linux GOARCH=arm64 $(XCADDY) build $(CADDY_VERSION) \
		$(CADDY_DNS_MODULES) \
		--output lib/ingress/binaries/caddy/$(CADDY_VERSION)/aarch64/caddy
	@chmod +x lib/ingress/binaries/caddy/$(CADDY_VERSION)/*/caddy
	@echo "Caddy binaries built successfully with DNS modules"

# Build Caddy for current architecture only (faster for development)
build-caddy: $(XCADDY)
	@echo "Building Caddy $(CADDY_VERSION) with DNS modules for current architecture..."
	@ARCH=$$(uname -m); \
	if [ "$$ARCH" = "x86_64" ]; then \
		CADDY_ARCH=x86_64; \
		GOARCH=amd64; \
	elif [ "$$ARCH" = "aarch64" ] || [ "$$ARCH" = "arm64" ]; then \
		CADDY_ARCH=aarch64; \
		GOARCH=arm64; \
	else \
		echo "Unsupported architecture: $$ARCH"; exit 1; \
	fi; \
	mkdir -p lib/ingress/binaries/caddy/$(CADDY_VERSION)/$$CADDY_ARCH; \
	GOOS=linux GOARCH=$$GOARCH $(XCADDY) build $(CADDY_VERSION) \
		$(CADDY_DNS_MODULES) \
		--output lib/ingress/binaries/caddy/$(CADDY_VERSION)/$$CADDY_ARCH/caddy; \
	chmod +x lib/ingress/binaries/caddy/$(CADDY_VERSION)/$$CADDY_ARCH/caddy
	@echo "Caddy binary built successfully"

# Download Cloud Hypervisor API spec
download-ch-spec:
	@echo "Downloading Cloud Hypervisor API spec..."
	@mkdir -p specs/cloud-hypervisor/api-v0.3.0
	@curl -L -o specs/cloud-hypervisor/api-v0.3.0/cloud-hypervisor.yaml \
		https://raw.githubusercontent.com/cloud-hypervisor/cloud-hypervisor/refs/tags/v48.0/vmm/src/api/openapi/cloud-hypervisor.yaml
	@echo "API spec downloaded"

# Generate Go code from OpenAPI spec
oapi-generate: $(OAPI_CODEGEN)
	@echo "Generating Go code from OpenAPI spec..."
	$(OAPI_CODEGEN) -config ./oapi-codegen.yaml ./openapi.yaml
	@echo "Formatting generated code..."
	go fmt ./lib/oapi/oapi.go

# Generate Cloud Hypervisor client from their OpenAPI spec
generate-vmm-client: $(OAPI_CODEGEN)
	@echo "Generating Cloud Hypervisor client from spec..."
	$(OAPI_CODEGEN) -config ./oapi-codegen-vmm.yaml ./specs/cloud-hypervisor/api-v0.3.0/cloud-hypervisor.yaml
	@echo "Formatting generated code..."
	go fmt ./lib/vmm/vmm.go

# Generate wire dependency injection code
generate-wire: $(WIRE)
	@echo "Generating wire code..."
	cd ./cmd/api && $(WIRE)

# Install proto generators from go.mod versions (pinned via tools.go)
install-proto-tools:
	@echo "Installing proto generators from go.mod versions..."
	go install google.golang.org/protobuf/cmd/protoc-gen-go
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc

# Generate gRPC code from proto
# Run 'make install-proto-tools' first to install generators from go.mod
generate-grpc: install-proto-tools
	@echo "Generating gRPC code from proto..."
	@echo "Using protoc-gen-go: $$(protoc-gen-go --version)"
	@echo "Using protoc-gen-go-grpc: $$(protoc-gen-go-grpc --version)"
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		lib/guest/guest.proto

# Generate all code
generate-all: oapi-generate generate-vmm-client generate-wire generate-grpc

# Check if CH binaries exist, download if missing
.PHONY: ensure-ch-binaries
ensure-ch-binaries:
	@ARCH=$$(uname -m); \
	if [ "$$ARCH" = "x86_64" ]; then \
		CH_ARCH=x86_64; \
	elif [ "$$ARCH" = "aarch64" ] || [ "$$ARCH" = "arm64" ]; then \
		CH_ARCH=aarch64; \
	else \
		echo "Unsupported architecture: $$ARCH"; exit 1; \
	fi; \
	if [ ! -f lib/vmm/binaries/cloud-hypervisor/v48.0/$$CH_ARCH/cloud-hypervisor ]; then \
		echo "Cloud Hypervisor binaries not found, downloading..."; \
		$(MAKE) download-ch-binaries; \
	fi

# Check if Caddy binaries exist, build if missing
.PHONY: ensure-caddy-binaries
ensure-caddy-binaries:
	@ARCH=$$(uname -m); \
	if [ "$$ARCH" = "x86_64" ]; then \
		CADDY_ARCH=x86_64; \
	elif [ "$$ARCH" = "aarch64" ] || [ "$$ARCH" = "arm64" ]; then \
		CADDY_ARCH=aarch64; \
	else \
		echo "Unsupported architecture: $$ARCH"; exit 1; \
	fi; \
	if [ ! -f lib/ingress/binaries/caddy/$(CADDY_VERSION)/$$CADDY_ARCH/caddy ]; then \
		echo "Caddy binary not found, building with xcaddy..."; \
		$(MAKE) build-caddy; \
	fi

# Build guest-agent (guest binary) into its own directory for embedding
# Cross-compile for Linux since it runs inside the VM
lib/system/guest_agent/guest-agent: lib/system/guest_agent/*.go
	@echo "Building guest-agent for Linux..."
	cd lib/system/guest_agent && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o guest-agent .

# Build init binary (runs as PID 1 in guest VM) for embedding
# Cross-compile for Linux since it runs inside the VM
lib/system/init/init: lib/system/init/*.go
	@echo "Building init binary for Linux..."
	cd lib/system/init && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o init .

build-embedded: lib/system/guest_agent/guest-agent lib/system/init/init

# Build the binary
build:
ifeq ($(shell uname -s),Darwin)
	$(MAKE) build-darwin
else
	$(MAKE) build-linux
endif

build-linux: ensure-ch-binaries ensure-caddy-binaries build-embedded | $(BIN_DIR)
	go build -tags containers_image_openpgp -o $(BIN_DIR)/hypeman ./cmd/api

# Build all binaries
build-all: build

# Run without live reload (build once and run)
run: build
	sudo setcap cap_net_admin,cap_net_bind_service=+eip $(BIN_DIR)/hypeman
	$(BIN_DIR)/hypeman

# Run in development mode with hot reload
# On macOS, redirects to dev-darwin which uses vz instead of cloud-hypervisor
dev:
	@if [ "$$(uname)" = "Darwin" ]; then \
		$(MAKE) dev-darwin; \
	else \
		$(MAKE) dev-linux; \
	fi

# Linux development mode with hot reload
dev-linux: ensure-ch-binaries ensure-caddy-binaries build-embedded $(AIR)
	@rm -f ./tmp/main
	$(AIR) -c .air.toml

# Run tests
# Usage: make test                              - runs all tests
#        make test TEST=TestCreateInstanceWithNetwork  - runs specific test
test:
ifeq ($(shell uname -s),Darwin)
	$(MAKE) test-darwin
else
	$(MAKE) test-linux
endif

# Linux tests (as root for network capabilities)
test-linux: ensure-ch-binaries ensure-caddy-binaries build-embedded
	@VERBOSE_FLAG=""; \
	TEST_PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$$PATH"; \
	if [ -n "$(VERBOSE)" ]; then VERBOSE_FLAG="-v"; fi; \
	if [ -n "$(TEST)" ]; then \
		echo "Running specific test: $(TEST)"; \
		sudo env "PATH=$$TEST_PATH" "DOCKER_CONFIG=$${DOCKER_CONFIG:-$$HOME/.docker}" go test -tags containers_image_openpgp -run=$(TEST) $$VERBOSE_FLAG -timeout=300s ./...; \
	else \
		sudo env "PATH=$$TEST_PATH" "DOCKER_CONFIG=$${DOCKER_CONFIG:-$$HOME/.docker}" go test -tags containers_image_openpgp $$VERBOSE_FLAG -timeout=300s ./...; \
	fi

# macOS tests (no sudo needed, adds e2fsprogs to PATH)
# Uses 'go list' to discover compilable packages, then filters out packages
# whose test files reference Linux-only symbols (network, devices, system/init).
DARWIN_EXCLUDE_PKGS := /lib/network|/lib/devices|/lib/system/init
test-darwin: build-embedded sign-vz-shim
	@VERBOSE_FLAG=""; \
	if [ -n "$(VERBOSE)" ]; then VERBOSE_FLAG="-v"; fi; \
	PKGS=$$(PATH="/opt/homebrew/opt/e2fsprogs/sbin:$(PATH)" \
		go list -tags containers_image_openpgp ./... 2>/dev/null | grep -Ev '$(DARWIN_EXCLUDE_PKGS)'); \
	if [ -n "$(TEST)" ]; then \
		echo "Running specific test: $(TEST)"; \
		PATH="/opt/homebrew/opt/e2fsprogs/sbin:$(PATH)" \
		go test -tags containers_image_openpgp -run=$(TEST) $$VERBOSE_FLAG -timeout=300s $$PKGS; \
	else \
		PATH="/opt/homebrew/opt/e2fsprogs/sbin:$(PATH)" \
		go test -tags containers_image_openpgp $$VERBOSE_FLAG -timeout=300s $$PKGS; \
	fi

# Generate JWT token for testing
# Usage: make gen-jwt [USER_ID=test-user]
# Checks CONFIG_PATH, then local config.yaml, then default config paths
gen-jwt:
	@CONFIG_PATH=$${CONFIG_PATH:-$$([ -f config.yaml ] && echo config.yaml)} go run ./cmd/gen-jwt -user-id $${USER_ID:-test-user}

# Build the generic builder image for builds
build-builder:
	docker build -t hypeman/builder:latest -f lib/builds/images/generic/Dockerfile .

# Alias for backwards compatibility
build-builders: build-builder

# Run E2E build system test (requires server running: make dev)
e2e-build-test:
	@./scripts/e2e-build-test.sh

# Clean generated files and binaries
clean:
	rm -rf $(BIN_DIR)
	rm -rf lib/vmm/binaries/cloud-hypervisor/
	rm -rf lib/ingress/binaries/
	rm -f lib/system/guest_agent/guest-agent
	rm -f lib/system/init/init
	rm -f lib/hypervisor/vz/vz-shim/vz-shim

# Prepare for release build (called by GoReleaser)
# Downloads all embedded binaries and builds embedded components
release-prep: download-ch-binaries build-caddy-binaries build-embedded
	go mod tidy

# =============================================================================
# macOS (vz/Virtualization.framework) targets
# =============================================================================

# Entitlements file for macOS codesigning
ENTITLEMENTS_FILE ?= vz.entitlements

# Build vz-shim (subprocess that hosts vz VMs)
# Also copies to embed directory so it gets embedded in the hypeman binary
.PHONY: build-vz-shim
build-vz-shim: | $(BIN_DIR)
	@echo "Building vz-shim for macOS..."
	go build -o $(BIN_DIR)/vz-shim ./cmd/vz-shim
	mkdir -p lib/hypervisor/vz/vz-shim
	cp $(BIN_DIR)/vz-shim lib/hypervisor/vz/vz-shim/vz-shim
	@echo "Build complete: $(BIN_DIR)/vz-shim"

# Sign vz-shim with entitlements
.PHONY: sign-vz-shim
sign-vz-shim: build-vz-shim
	@echo "Signing $(BIN_DIR)/vz-shim with entitlements..."
	codesign --sign - --entitlements $(ENTITLEMENTS_FILE) --force $(BIN_DIR)/vz-shim
	@echo "Signed: $(BIN_DIR)/vz-shim"

# Build for macOS with vz support
# Note: This builds without embedded CH/Caddy binaries since vz doesn't need them
# Guest-agent and init are cross-compiled for Linux (they run inside the VM)
.PHONY: build-darwin
build-darwin: build-embedded build-vz-shim | $(BIN_DIR)
	@echo "Building hypeman for macOS with vz support..."
	go build -tags containers_image_openpgp -o $(BIN_DIR)/hypeman ./cmd/api
	@echo "Build complete: $(BIN_DIR)/hypeman"

# Sign the binary with entitlements (required for Virtualization.framework)
# Usage: make sign-darwin
.PHONY: sign-darwin
sign-darwin: build-darwin sign-vz-shim
	@echo "Signing $(BIN_DIR)/hypeman with entitlements..."
	codesign --sign - --entitlements $(ENTITLEMENTS_FILE) --force $(BIN_DIR)/hypeman
	@echo "Verifying signature..."
	codesign --display --entitlements - $(BIN_DIR)/hypeman

# Sign with a specific identity (for distribution)
# Usage: make sign-darwin-identity IDENTITY="Developer ID Application: Your Name"
.PHONY: sign-darwin-identity
sign-darwin-identity: build-darwin
	@if [ -z "$(IDENTITY)" ]; then \
		echo "Error: IDENTITY not set. Usage: make sign-darwin-identity IDENTITY='Developer ID Application: ...'"; \
		exit 1; \
	fi
	@echo "Signing $(BIN_DIR)/hypeman with identity: $(IDENTITY)"
	codesign --sign "$(IDENTITY)" --entitlements $(ENTITLEMENTS_FILE) --force --options runtime $(BIN_DIR)/hypeman
	@echo "Verifying signature..."
	codesign --verify --verbose $(BIN_DIR)/hypeman

# Run on macOS with vz support (development mode)
# Automatically signs the binary before running
.PHONY: dev-darwin
# macOS development mode with hot reload (uses vz, no sudo needed)
dev-darwin: build-embedded $(AIR)
	@rm -f ./tmp/main
	PATH="/opt/homebrew/opt/e2fsprogs/sbin:$(PATH)" $(AIR) -c .air.darwin.toml

# Run without hot reload (for agents)
run:
	@if [ "$$(uname)" = "Darwin" ]; then \
		$(MAKE) run-darwin; \
	else \
		$(MAKE) run-linux; \
	fi

run-linux: ensure-ch-binaries ensure-caddy-binaries build-embedded build
	./bin/hypeman

run-darwin: sign-darwin
	PATH="/opt/homebrew/opt/e2fsprogs/sbin:$(PATH)" ./bin/hypeman

# Quick test of vz package compilation
.PHONY: test-vz-compile
test-vz-compile:
	@echo "Testing vz package compilation..."
	go build ./lib/hypervisor/vz/...
	@echo "vz package compiles successfully"

# Verify entitlements on a signed binary
.PHONY: verify-entitlements
verify-entitlements:
	@if [ ! -f $(BIN_DIR)/hypeman ]; then \
		echo "Error: $(BIN_DIR)/hypeman not found. Run 'make sign-darwin' first."; \
		exit 1; \
	fi
	@echo "Entitlements on $(BIN_DIR)/hypeman:"
	codesign --display --entitlements - $(BIN_DIR)/hypeman
