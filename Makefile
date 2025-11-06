SHELL := /bin/bash
.PHONY: oapi-generate dev build test install-tools

# Directory where local binaries will be installed
BIN_DIR ?= $(CURDIR)/bin

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

# Local binary paths
OAPI_CODEGEN ?= $(BIN_DIR)/oapi-codegen
AIR ?= $(BIN_DIR)/air

# Install oapi-codegen
$(OAPI_CODEGEN): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest

# Install air for hot reload
$(AIR): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/air-verse/air@latest

install-tools: $(OAPI_CODEGEN) $(AIR)

# Generate Go code from OpenAPI spec
oapi-generate: $(OAPI_CODEGEN)
	@echo "Generating Go code from OpenAPI spec..."
	$(OAPI_CODEGEN) -config ./oapi-codegen.yaml ./openapi.yaml
	@echo "Formatting generated code..."
	go fmt ./lib/oapi/oapi.go

# Build the binary
build: | $(BIN_DIR)
	go build -o $(BIN_DIR)/dataplane ./cmd/dataplane

# Run in development mode with hot reload
dev: $(AIR)
	$(AIR) -c .air.toml

# Run tests
test:
	go test -v ./...

# Clean generated files and binaries
clean:
	rm -rf $(BIN_DIR)
	rm -f lib/oapi/oapi.go

