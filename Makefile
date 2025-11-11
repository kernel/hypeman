SHELL := /bin/bash
.PHONY: oapi-generate generate-wire generate-all dev build test install-tools gen-jwt

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

# Generate Go code from OpenAPI spec
oapi-generate: $(OAPI_CODEGEN)
	@echo "Generating Go code from OpenAPI spec..."
	$(OAPI_CODEGEN) -config ./oapi-codegen.yaml ./openapi.yaml
	@echo "Formatting generated code..."
	go fmt ./lib/oapi/oapi.go

# Generate wire dependency injection code
generate-wire: $(WIRE)
	@echo "Generating wire code..."
	cd ./cmd/api && $(WIRE)

# Generate all code
generate-all: oapi-generate generate-wire

# Build the binary
build: | $(BIN_DIR)
	go build -tags containers_image_openpgp -o $(BIN_DIR)/hypeman ./cmd/api

# Run in development mode with hot reload
dev: $(AIR)
	$(AIR) -c .air.toml

# Run tests
test:
	go test -tags containers_image_openpgp -tags containers_image_openpgp -v -timeout 30s ./...

# Generate JWT token for testing
# Usage: make gen-jwt [USER_ID=test-user]
gen-jwt: $(GODOTENV)
	@$(GODOTENV) -f .env go run ./cmd/gen-jwt -user-id $${USER_ID:-test-user}

# Clean generated files and binaries
clean:
	rm -rf $(BIN_DIR)
	rm -f lib/oapi/oapi.go

