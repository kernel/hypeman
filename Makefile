SHELL := /bin/bash
.PHONY: oapi-generate generate-vmm-client generate-wire generate-all dev build test install-tools gen-jwt download-ch-binaries download-ch-spec ensure-ch-binaries download-envoy-binaries ensure-envoy-binaries

# Directory where local binaries will be installed
BIN_DIR ?= $(CURDIR)/bin

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

# Local binary paths
OAPI_CODEGEN ?= $(BIN_DIR)/oapi-codegen
AIR ?= $(BIN_DIR)/air
WIRE ?= $(BIN_DIR)/wire
GODOTENV ?= $(BIN_DIR)/godotenv

# Install oapi-codegen
$(OAPI_CODEGEN): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest

# Install air for hot reload
$(AIR): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/air-verse/air@latest

# Install wire for dependency injection
$(WIRE): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/google/wire/cmd/wire@latest

# Install godotenv for loading .env files
$(GODOTENV): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/joho/godotenv/cmd/godotenv@latest

install-tools: $(OAPI_CODEGEN) $(AIR) $(WIRE) $(GODOTENV)

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

# Download Envoy binaries
download-envoy-binaries:
	@echo "Downloading Envoy binaries..."
	@mkdir -p lib/ingress/binaries/envoy/v1.36/{x86_64,aarch64}
	@echo "Downloading Envoy v1.36.3 for x86_64..."
	@curl -L -o lib/ingress/binaries/envoy/v1.36/x86_64/envoy \
		https://github.com/envoyproxy/envoy/releases/download/v1.36.3/envoy-1.36.3-linux-x86_64
	@echo "Downloading Envoy v1.36.3 for aarch64..."
	@curl -L -o lib/ingress/binaries/envoy/v1.36/aarch64/envoy \
		https://github.com/envoyproxy/envoy/releases/download/v1.36.3/envoy-1.36.3-linux-aarch_64
	@chmod +x lib/ingress/binaries/envoy/v1.36/*/envoy
	@echo "Envoy binaries downloaded successfully"

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

# Generate gRPC code from proto
generate-grpc:
	@echo "Generating gRPC code from proto..."
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		lib/exec/exec.proto

# Generate all code
generate-all: oapi-generate generate-vmm-client generate-wire generate-grpc

# Check if CH binaries exist, download if missing
.PHONY: ensure-ch-binaries
ensure-ch-binaries:
	@if [ ! -f lib/vmm/binaries/cloud-hypervisor/v48.0/x86_64/cloud-hypervisor ]; then \
		echo "Cloud Hypervisor binaries not found, downloading..."; \
		$(MAKE) download-ch-binaries; \
	fi

# Check if Envoy binaries exist, download if missing
.PHONY: ensure-envoy-binaries
ensure-envoy-binaries:
	@if [ ! -f lib/ingress/binaries/envoy/v1.36/x86_64/envoy ]; then \
		echo "Envoy binaries not found, downloading..."; \
		$(MAKE) download-envoy-binaries; \
	fi

# Build exec-agent (guest binary) into its own directory for embedding
lib/system/exec_agent/exec-agent: lib/system/exec_agent/main.go
	@echo "Building exec-agent..."
	cd lib/system/exec_agent && CGO_ENABLED=0 go build -ldflags="-s -w" -o exec-agent .

# Build the binary
build: ensure-ch-binaries ensure-envoy-binaries lib/system/exec_agent/exec-agent | $(BIN_DIR)
	go build -tags containers_image_openpgp -o $(BIN_DIR)/hypeman ./cmd/api

# Build exec CLI
build-exec: | $(BIN_DIR)
	go build -o $(BIN_DIR)/hypeman-exec ./cmd/exec

# Build all binaries
build-all: build build-exec

# Run in development mode with hot reload
dev: $(AIR)
	$(AIR) -c .air.toml

# Run tests (as root for network capabilities, enables caching and parallelism)
# Usage: make test                              - runs all tests
#        make test TEST=TestCreateInstanceWithNetwork  - runs specific test
test: ensure-ch-binaries ensure-caddy-binaries lib/system/exec_agent/exec-agent
	@if [ -n "$(TEST)" ]; then \
		echo "Running specific test: $(TEST)"; \
		sudo env "PATH=$$PATH" go test -tags containers_image_openpgp -run=$(TEST) -v -timeout=180s ./...; \
	else \
		sudo env "PATH=$$PATH" go test -tags containers_image_openpgp -v -timeout=180s ./...; \
	fi

# Generate JWT token for testing
# Usage: make gen-jwt [USER_ID=test-user]
gen-jwt: $(GODOTENV)
	@$(GODOTENV) -f .env go run ./cmd/gen-jwt -user-id $${USER_ID:-test-user}

# Clean generated files and binaries
clean:
	rm -rf $(BIN_DIR)
	rm -f lib/oapi/oapi.go
	rm -f lib/vmm/vmm.go
	rm -f lib/exec/exec.pb.go
	rm -f lib/exec/exec_grpc.pb.go
	rm -f lib/system/exec_agent/exec-agent
