#!/bin/bash
#
# Hypeman CLI Install Script
#
# Installs only the hypeman CLI (kernel/hypeman-cli). It does not install or
# configure the hypeman server.
#
# Usage:
#   curl -fsSL https://get.hypeman.sh/cli | bash
#
# Options (via environment variables):
#   CLI_VERSION  - Install a specific CLI version (default: latest)
#   INSTALL_DIR  - Binary installation directory (default: /usr/local/bin,
#                  falling back to ~/.local/bin when not writable)
#

set -e

REPO="kernel/hypeman-cli"
BINARY_NAME="hypeman"

# Colors for output (true color)
RED='\033[38;2;255;110;110m'
GREEN='\033[38;2;92;190;83m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

info() { echo -e "${GREEN}[INFO]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

# Find the most recent release that has a specific artifact available
# Usage: find_release_with_artifact <repo> <archive_prefix> <os> <arch> [ext]
# Returns: version tag (e.g., v0.5.0) or empty string if not found
find_release_with_artifact() {
    local repo="$1"
    local archive_prefix="$2"
    local os="$3"
    local arch="$4"
    local ext="${5:-tar.gz}"

    local tags
    tags=$(curl -fsSL "https://api.github.com/repos/${repo}/releases?per_page=10" 2>/dev/null | grep '"tag_name"' | cut -d'"' -f4)
    if [ -z "$tags" ]; then
        return 1
    fi

    for tag in $tags; do
        local version_num="${tag#v}"
        local artifact_name="${archive_prefix}_${version_num}_${os}_${arch}.${ext}"
        local artifact_url="https://github.com/${repo}/releases/download/${tag}/${artifact_name}"

        if curl -fsSL --head "$artifact_url" >/dev/null 2>&1; then
            echo "$tag"
            return 0
        fi
    done

    return 1
}

# =============================================================================
# Detect OS and architecture
# =============================================================================

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case $ARCH in
    x86_64|amd64)
        ARCH="amd64"
        ;;
    aarch64|arm64)
        ARCH="arm64"
        ;;
    *)
        error "Unsupported architecture: $ARCH (supported: amd64, arm64)"
        ;;
esac

if [ "$OS" != "linux" ] && [ "$OS" != "darwin" ]; then
    error "Unsupported OS: $OS (supported: linux, darwin)"
fi

# CLI releases use goreleaser naming: "macos" not "darwin", .zip not .tar.gz on macOS
if [ "$OS" = "darwin" ]; then
    CLI_OS="macos"
    CLI_EXT="zip"
else
    CLI_OS="$OS"
    CLI_EXT="tar.gz"
fi

# =============================================================================
# Resolve installation directory
# =============================================================================

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
SUDO=""
if [ -w "$INSTALL_DIR" ]; then
    :
elif command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
else
    INSTALL_DIR="$HOME/.local/bin"
fi
$SUDO mkdir -p "$INSTALL_DIR"

# =============================================================================
# Resolve version and download
# =============================================================================

if [ -z "${CLI_VERSION:-}" ] || [ "$CLI_VERSION" = "latest" ]; then
    info "Fetching latest CLI release for ${CLI_OS}/${ARCH}..."
    CLI_VERSION=$(find_release_with_artifact "$REPO" "$BINARY_NAME" "$CLI_OS" "$ARCH" "$CLI_EXT" || true)
    [ -n "$CLI_VERSION" ] || error "Could not find a CLI release with an artifact for ${CLI_OS}/${ARCH}"
fi

CLI_VERSION_NUM="${CLI_VERSION#v}"
ARCHIVE_NAME="${BINARY_NAME}_${CLI_VERSION_NUM}_${CLI_OS}_${ARCH}.${CLI_EXT}"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${CLI_VERSION}/${ARCHIVE_NAME}"

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

info "Installing hypeman CLI ${CLI_VERSION}..."
curl -fsSL "$DOWNLOAD_URL" -o "${TMP_DIR}/${ARCHIVE_NAME}" || error "Failed to download ${DOWNLOAD_URL}"

mkdir -p "${TMP_DIR}/cli"
if [ "$CLI_EXT" = "zip" ]; then
    unzip -qo "${TMP_DIR}/${ARCHIVE_NAME}" -d "${TMP_DIR}/cli"
else
    tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "${TMP_DIR}/cli"
fi

[ -f "${TMP_DIR}/cli/${BINARY_NAME}" ] || error "Archive did not contain expected '${BINARY_NAME}' binary"

$SUDO install -m 755 "${TMP_DIR}/cli/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"

info "Installed ${BINARY_NAME} to ${INSTALL_DIR}/${BINARY_NAME}"

case ":$PATH:" in
    *":${INSTALL_DIR}:"*) ;;
    *) warn "${INSTALL_DIR} is not on your PATH. Add it with: export PATH=\"${INSTALL_DIR}:\$PATH\"" ;;
esac

info "Run 'hypeman --help' to get started."
