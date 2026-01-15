#!/bin/bash
#
# Hypeman Build from Source Script
#
# Usage:
#   ./scripts/build-from-source.sh
#
# Options (via environment variables):
#   OUTPUT_DIR   - Full path of directory to place built binaries (optional, default: repo's root bin directory)
#

set -euo pipefail

# Default values
BINARY_NAME="hypeman-api"

# Colors for output (true color)
RED='\033[38;2;255;110;110m'
GREEN='\033[38;2;92;190;83m'
YELLOW='\033[0;33m'
PURPLE='\033[38;2;172;134;249m'
NC='\033[0m' # No Color

info() { echo -e "${GREEN}[INFO]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

# =============================================================================
# Pre-flight checks - verify all requirements before doing anything
# =============================================================================

# Check for required commands
command -v go >/dev/null 2>&1 || error "go is required but not installed"
command -v make >/dev/null 2>&1 || error "make is required but not installed"

# =============================================================================
# Setup directories
# =============================================================================

# Get the directory where this script is located
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

: "${OUTPUT_DIR:=${SOURCE_DIR}/bin}"

# Create output directory if it doesn't exist
mkdir -p "$OUTPUT_DIR"

BUILD_LOG="${OUTPUT_DIR}/build.log"
: > "$BUILD_LOG"

# =============================================================================
# Build from source
# =============================================================================

info "Building from source (${SOURCE_DIR})..."

info "Building binaries (this may take a few minutes)..."
cd "${SOURCE_DIR}"

# Build main binary (includes dependencies) - capture output, show on error
if ! make build >> "$BUILD_LOG" 2>&1; then
    echo ""
    echo -e "${RED}Build failed. Full build log:${NC}"
    cat "$BUILD_LOG"
    error "Build failed"
fi
cp "bin/hypeman" "${OUTPUT_DIR}/${BINARY_NAME}"

# Build hypeman-token (not included in make build)
if ! go build -o "${OUTPUT_DIR}/hypeman-token" ./cmd/gen-jwt >> "$BUILD_LOG" 2>&1; then
    echo ""
    echo -e "${RED}Build failed. Full build log:${NC}"
    cat "$BUILD_LOG"
    error "Failed to build hypeman-token"
fi

# Copy .env.example for config template
cp ".env.example" "${OUTPUT_DIR}/.env.example"

info "Build complete"
info "Binaries are available in: ${OUTPUT_DIR}"
