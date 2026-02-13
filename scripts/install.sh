#!/bin/bash
#
# Hypeman API Install Script
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/kernel/hypeman/main/scripts/install.sh | bash
#
# Options (via environment variables):
#   VERSION      - Install specific API version (default: latest)
#   CLI_VERSION  - Install specific CLI version (default: latest)
#   BRANCH       - Build from source using this branch (for development/testing)
#   BINARY_DIR   - Use binaries from this directory instead of building/downloading
#   INSTALL_DIR  - Binary installation directory (default: /opt/hypeman/bin on Linux, /usr/local/bin on macOS)
#   DATA_DIR     - Data directory (default: /var/lib/hypeman on Linux, ~/Library/Application Support/hypeman on macOS)
#   CONFIG_DIR   - Config directory (default: /etc/hypeman on Linux, ~/.config/hypeman on macOS)
#

set -e

REPO="kernel/hypeman"
BINARY_NAME="hypeman-api"
SERVICE_NAME="hypeman"

# Colors for output (true color)
RED='\033[38;2;255;110;110m'
GREEN='\033[38;2;92;190;83m'
YELLOW='\033[0;33m'
PURPLE='\033[38;2;172;134;249m'
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

    # Fetch recent release tags (up to 10)
    local tags
    tags=$(curl -fsSL "https://api.github.com/repos/${repo}/releases?per_page=10" 2>/dev/null | grep '"tag_name"' | cut -d'"' -f4)
    if [ -z "$tags" ]; then
        return 1
    fi

    # Check each release for the artifact
    for tag in $tags; do
        local version_num="${tag#v}"
        local artifact_name="${archive_prefix}_${version_num}_${os}_${arch}.${ext}"
        local artifact_url="https://github.com/${repo}/releases/download/${tag}/${artifact_name}"

        # Check if artifact exists (follow redirects, fail silently)
        if curl -fsSL --head "$artifact_url" >/dev/null 2>&1; then
            echo "$tag"
            return 0
        fi
    done

    return 1
}

# =============================================================================
# Detect OS and architecture (before pre-flight checks)
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

# =============================================================================
# OS-conditional defaults
# =============================================================================

if [ "$OS" = "darwin" ]; then
    INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
    DATA_DIR="${DATA_DIR:-$HOME/Library/Application Support/hypeman}"
    CONFIG_DIR="${CONFIG_DIR:-$HOME/.config/hypeman}"
else
    INSTALL_DIR="${INSTALL_DIR:-/opt/hypeman/bin}"
    DATA_DIR="${DATA_DIR:-/var/lib/hypeman}"
    CONFIG_DIR="${CONFIG_DIR:-/etc/hypeman}"
fi

CONFIG_FILE="${CONFIG_DIR}/config"
SYSTEMD_DIR="/etc/systemd/system"

# =============================================================================
# Pre-flight checks - verify all requirements before doing anything
# =============================================================================

info "Running pre-flight checks..."

SUDO=""
if [ "$OS" = "darwin" ]; then
    # macOS pre-flight
    if [ "$ARCH" != "arm64" ]; then
        error "Intel Macs not supported"
    fi
    command -v codesign >/dev/null 2>&1 || error "codesign is required but not installed (install Xcode Command Line Tools)"
    command -v docker >/dev/null 2>&1 || error "Docker CLI is required but not found. Install Docker via Colima or Docker Desktop."
    # Check if we need sudo for INSTALL_DIR
    if [ ! -w "$INSTALL_DIR" ] 2>/dev/null && [ ! -w "$(dirname "$INSTALL_DIR")" ] 2>/dev/null; then
        if command -v sudo >/dev/null 2>&1; then
            if ! sudo -n true 2>/dev/null; then
                info "Requesting sudo privileges (needed for $INSTALL_DIR)..."
                if ! sudo -v < /dev/tty; then
                    error "Failed to obtain sudo privileges"
                fi
            fi
            SUDO="sudo"
        fi
    fi
else
    # Linux pre-flight
    if [ "$EUID" -ne 0 ]; then
        if ! command -v sudo >/dev/null 2>&1; then
            error "This script requires root privileges. Please run as root or install sudo."
        fi
        if ! sudo -n true 2>/dev/null; then
            info "Requesting sudo privileges..."
            if ! sudo -v < /dev/tty; then
                error "Failed to obtain sudo privileges"
            fi
        fi
        SUDO="sudo"
    fi
    command -v systemctl >/dev/null 2>&1 || error "systemctl is required but not installed (systemd not available?)"
fi

# Common checks
command -v curl >/dev/null 2>&1 || error "curl is required but not installed"
command -v tar >/dev/null 2>&1 || error "tar is required but not installed"
command -v openssl >/dev/null 2>&1 || error "openssl is required but not installed"

# Count how many of BRANCH, VERSION, BINARY_DIR are set
count=0
[ -n "$BRANCH" ] && ((count++)) || true
[ -n "$VERSION" ] && ((count++)) || true
[ -n "$BINARY_DIR" ] && ((count++)) || true

if [ "$count" -gt 1 ]; then
    error "BRANCH, VERSION, and BINARY_DIR are mutually exclusive"
fi

# Additional checks for build-from-source mode
if [ -n "$BRANCH" ]; then
    command -v git >/dev/null 2>&1 || error "git is required for BRANCH mode but not installed"
    command -v go >/dev/null 2>&1 || error "go is required for BRANCH mode but not installed"
    command -v make >/dev/null 2>&1 || error "make is required for BRANCH mode but not installed"
fi

# Additional checks for BINARY_DIR mode
if [ -n "$BINARY_DIR" ]; then
    if [ ! -d "$BINARY_DIR" ]; then
        error "BINARY_DIR does not exist: ${BINARY_DIR}. Are you sure you provided the correct path?"
    fi
fi

info "Pre-flight checks passed"

# =============================================================================
# System Configuration - KVM access and network capabilities
# =============================================================================

INSTALL_USER="${SUDO_USER:-$(whoami)}"

if [ "$OS" = "darwin" ]; then
    info "macOS uses NAT networking via Virtualization.framework, no system config needed"
else
    # Ensure KVM access
    if [ -e /dev/kvm ]; then
        if getent group kvm &>/dev/null; then
            if ! groups "$INSTALL_USER" 2>/dev/null | grep -qw kvm; then
                info "Adding user ${INSTALL_USER} to kvm group..."
                $SUDO usermod -aG kvm "$INSTALL_USER"
                warn "You may need to log out and back in for kvm group membership to take effect"
            fi
        fi
    else
        warn "/dev/kvm not found - KVM may not be available on this system"
    fi

    # Enable IPv4 forwarding (required for VM networking)
    CURRENT_IP_FORWARD=$(sysctl -n net.ipv4.ip_forward 2>/dev/null || echo "0")
    if [ "$CURRENT_IP_FORWARD" != "1" ]; then
        info "Enabling IPv4 forwarding..."
        $SUDO sysctl -w net.ipv4.ip_forward=1 > /dev/null

        # Make it persistent across reboots
        if [ -d /etc/sysctl.d ]; then
            echo 'net.ipv4.ip_forward=1' | $SUDO tee /etc/sysctl.d/99-hypeman.conf > /dev/null
        elif ! grep -q '^net.ipv4.ip_forward=1' /etc/sysctl.conf 2>/dev/null; then
            echo 'net.ipv4.ip_forward=1' | $SUDO tee -a /etc/sysctl.conf > /dev/null
        fi
    fi

    # Increase file descriptor limit for Caddy (ingress)
    if [ -d /etc/security/limits.d ]; then
        if [ ! -f /etc/security/limits.d/99-hypeman.conf ]; then
            info "Configuring file descriptor limits for ingress..."
            $SUDO tee /etc/security/limits.d/99-hypeman.conf > /dev/null << 'LIMITS'
# Hypeman: Increased file descriptor limits for Caddy ingress
*  soft  nofile  65536
*  hard  nofile  65536
root  soft  nofile  65536
root  hard  nofile  65536
LIMITS
        fi
    fi
fi

# =============================================================================
# Create temp directory
# =============================================================================

TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR" EXIT

# =============================================================================
# Get binaries (either use BINARY_DIR, download release, or build from source)
# =============================================================================

if [ -n "$BINARY_DIR" ]; then
    # Use binaries from specified directory
    info "Using binaries from ${BINARY_DIR}..."

    # Copy binaries to TMP_DIR
    info "Copying binaries from ${BINARY_DIR}..."

    if [ "$OS" = "darwin" ]; then
        for f in "${BINARY_NAME}" "hypeman-token" ".env.darwin.example"; do
            [ -f "${BINARY_DIR}/${f}" ] || error "File ${f} not found in ${BINARY_DIR}"
        done
        cp "${BINARY_DIR}/.env.darwin.example" "${TMP_DIR}/.env.darwin.example"
    else
        for f in "${BINARY_NAME}" "hypeman-token" ".env.example"; do
            [ -f "${BINARY_DIR}/${f}" ] || error "File ${f} not found in ${BINARY_DIR}"
        done
        cp "${BINARY_DIR}/.env.example" "${TMP_DIR}/.env.example"
    fi

    cp "${BINARY_DIR}/${BINARY_NAME}" "${TMP_DIR}/${BINARY_NAME}"
    cp "${BINARY_DIR}/hypeman-token" "${TMP_DIR}/hypeman-token"

    # Make binaries executable
    chmod +x "${TMP_DIR}/${BINARY_NAME}"
    chmod +x "${TMP_DIR}/hypeman-token"

    VERSION="custom (from binary)"
elif [ -n "$BRANCH" ]; then
    # Build from source mode
    info "Building from source (branch: $BRANCH)..."

    BUILD_DIR="${TMP_DIR}/hypeman"
    BUILD_LOG="${TMP_DIR}/build.log"

    # Clone repo (quiet)
    if ! git clone --branch "$BRANCH" --depth 1 -q "https://github.com/${REPO}.git" "$BUILD_DIR" 2>&1 | tee -a "$BUILD_LOG"; then
        error "Failed to clone repository. Build log:\n$(cat "$BUILD_LOG")"
    fi

    info "Building binaries (this may take a few minutes)..."
    cd "$BUILD_DIR"

    if ! make build >> "$BUILD_LOG" 2>&1; then
        echo ""
        echo -e "${RED}Build failed. Full build log:${NC}"
        cat "$BUILD_LOG"
        error "Build failed"
    fi
    if [ "$OS" = "darwin" ]; then
        if ! make sign-darwin >> "$BUILD_LOG" 2>&1; then
            echo ""
            echo -e "${RED}Signing failed. Full build log:${NC}"
            cat "$BUILD_LOG"
            error "Signing failed"
        fi
        cp ".env.darwin.example" "${TMP_DIR}/.env.darwin.example"
    else
        cp ".env.example" "${TMP_DIR}/.env.example"
    fi
    cp "bin/hypeman" "${TMP_DIR}/${BINARY_NAME}"

    # Build hypeman-token (not included in make build)
    if ! go build -o "${TMP_DIR}/hypeman-token" ./cmd/gen-jwt >> "$BUILD_LOG" 2>&1; then
        echo ""
        echo -e "${RED}Build failed. Full build log:${NC}"
        cat "$BUILD_LOG"
        error "Failed to build hypeman-token"
    fi

    VERSION="$BRANCH (source)"
    cd - > /dev/null

    info "Build complete"
else
    # Download release mode
    if [ -z "$VERSION" ]; then
        info "Fetching latest version with available artifacts..."
        VERSION=$(find_release_with_artifact "$REPO" "hypeman" "$OS" "$ARCH")
        if [ -z "$VERSION" ]; then
            error "Failed to find a release with artifacts for ${OS}/${ARCH}"
        fi
    fi
    info "Installing version: $VERSION"

    # Construct download URL
    VERSION_NUM="${VERSION#v}"
    ARCHIVE_NAME="hypeman_${VERSION_NUM}_${OS}_${ARCH}.tar.gz"
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE_NAME}"

    info "Downloading ${ARCHIVE_NAME}..."
    if ! curl -fsSL "$DOWNLOAD_URL" -o "${TMP_DIR}/${ARCHIVE_NAME}"; then
        error "Failed to download from ${DOWNLOAD_URL}"
    fi

    info "Extracting..."
    tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "$TMP_DIR"

    # On macOS, codesign after extraction with virtualization entitlements
    if [ "$OS" = "darwin" ]; then
        info "Signing binaries..."
        ENTITLEMENTS_TMP="${TMP_DIR}/vz.entitlements"
        cat > "$ENTITLEMENTS_TMP" << 'ENTITLEMENTS'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>com.apple.security.virtualization</key>
	<true/>
	<key>com.apple.security.network.server</key>
	<true/>
	<key>com.apple.security.network.client</key>
	<true/>
</dict>
</plist>
ENTITLEMENTS
        if ! codesign --force --sign - --entitlements "$ENTITLEMENTS_TMP" "${TMP_DIR}/${BINARY_NAME}" 2>/dev/null; then
            warn "codesign failed — vz hypervisor will not work without virtualization entitlement"
        fi
        rm -f "$ENTITLEMENTS_TMP"
    fi
fi

# =============================================================================
# Stop existing service if running
# =============================================================================

if [ "$OS" = "darwin" ]; then
    PLIST_PATH="$HOME/Library/LaunchAgents/com.kernel.hypeman.plist"
    if [ -f "$PLIST_PATH" ]; then
        info "Stopping existing ${SERVICE_NAME} service..."
        launchctl unload "$PLIST_PATH" 2>/dev/null || true
    fi
else
    if $SUDO systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        info "Stopping existing ${SERVICE_NAME} service..."
        $SUDO systemctl stop "$SERVICE_NAME"
    fi
fi

# =============================================================================
# Install binaries
# =============================================================================

info "Installing ${BINARY_NAME} to ${INSTALL_DIR}..."
$SUDO mkdir -p "$INSTALL_DIR"
$SUDO install -m 755 "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"

# Install hypeman-token binary
info "Installing hypeman-token to ${INSTALL_DIR}..."
$SUDO install -m 755 "${TMP_DIR}/hypeman-token" "${INSTALL_DIR}/hypeman-token"

if [ "$OS" = "linux" ]; then
    # Install wrapper script to /usr/local/bin for easy access
    info "Installing hypeman-token wrapper to /usr/local/bin..."
    $SUDO tee /usr/local/bin/hypeman-token > /dev/null << EOF
#!/bin/bash
# Wrapper script for hypeman-token that loads config from ${CONFIG_FILE}
set -a
source ${CONFIG_FILE}
set +a
exec ${INSTALL_DIR}/hypeman-token "\$@"
EOF
    $SUDO chmod 755 /usr/local/bin/hypeman-token
fi

# =============================================================================
# Create directories
# =============================================================================

info "Creating data directory at ${DATA_DIR}..."
if [ "$OS" = "darwin" ]; then
    mkdir -p "$DATA_DIR"
    mkdir -p "$DATA_DIR/logs"
else
    $SUDO mkdir -p "$DATA_DIR"
fi

info "Creating config directory at ${CONFIG_DIR}..."
if [ "$OS" = "darwin" ]; then
    mkdir -p "$CONFIG_DIR"
else
    $SUDO mkdir -p "$CONFIG_DIR"
fi

# =============================================================================
# Create config file (if it doesn't exist)
# =============================================================================

if [ ! -f "$CONFIG_FILE" ]; then
    if [ "$OS" = "darwin" ]; then
        # macOS config
        if [ -f "${TMP_DIR}/.env.darwin.example" ]; then
            info "Using macOS config template from source..."
            cp "${TMP_DIR}/.env.darwin.example" "${TMP_DIR}/config"
        else
            info "Downloading macOS config template..."
            CONFIG_URL="https://raw.githubusercontent.com/${REPO}/${VERSION}/.env.darwin.example"
            if ! curl -fsSL "$CONFIG_URL" -o "${TMP_DIR}/config"; then
                error "Failed to download config template from ${CONFIG_URL}"
            fi
        fi

        # Expand ~ to $HOME (launchd doesn't do shell expansion)
        sed -i '' "s|~/|${HOME}/|g" "${TMP_DIR}/config"

        # Generate random JWT secret
        info "Generating JWT secret..."
        JWT_SECRET=$(openssl rand -hex 32)
        sed -i '' "s/^JWT_SECRET=.*/JWT_SECRET=${JWT_SECRET}/" "${TMP_DIR}/config"

        # Auto-detect Docker socket
        DOCKER_SOCKET=""
        if [ -n "$DOCKER_HOST" ]; then
            DOCKER_SOCKET="${DOCKER_HOST#unix://}"
        elif [ -S /var/run/docker.sock ]; then
            DOCKER_SOCKET="/var/run/docker.sock"
        elif [ -S "$HOME/.colima/default/docker.sock" ]; then
            DOCKER_SOCKET="$HOME/.colima/default/docker.sock"
        fi
        if [ -n "$DOCKER_SOCKET" ]; then
            info "Detected Docker socket: ${DOCKER_SOCKET}"
            if grep -q '^DOCKER_SOCKET=' "${TMP_DIR}/config"; then
                sed -i '' "s|^DOCKER_SOCKET=.*|DOCKER_SOCKET=${DOCKER_SOCKET}|" "${TMP_DIR}/config"
            elif grep -q '^# DOCKER_SOCKET=' "${TMP_DIR}/config"; then
                sed -i '' "s|^# DOCKER_SOCKET=.*|DOCKER_SOCKET=${DOCKER_SOCKET}|" "${TMP_DIR}/config"
            else
                echo "DOCKER_SOCKET=${DOCKER_SOCKET}" >> "${TMP_DIR}/config"
            fi
        fi

        info "Installing config file at ${CONFIG_FILE}..."
        install -m 600 "${TMP_DIR}/config" "$CONFIG_FILE"
    else
        # Linux config
        if [ -f "${TMP_DIR}/.env.example" ]; then
            info "Using config template from source..."
            cp "${TMP_DIR}/.env.example" "${TMP_DIR}/config"
        else
            info "Downloading config template..."
            CONFIG_URL="https://raw.githubusercontent.com/${REPO}/${VERSION}/.env.example"
            if ! curl -fsSL "$CONFIG_URL" -o "${TMP_DIR}/config"; then
                error "Failed to download config template from ${CONFIG_URL}"
            fi
        fi

        # Generate random JWT secret
        info "Generating JWT secret..."
        JWT_SECRET=$(openssl rand -hex 32)
        sed -i "s/^JWT_SECRET=$/JWT_SECRET=${JWT_SECRET}/" "${TMP_DIR}/config"

        # Set fixed ports for production (instead of random ports used in dev)
        sed -i "s/^# CADDY_ADMIN_PORT=.*/CADDY_ADMIN_PORT=2019/" "${TMP_DIR}/config"
        sed -i "s/^# INTERNAL_DNS_PORT=.*/INTERNAL_DNS_PORT=5353/" "${TMP_DIR}/config"

        info "Installing config file at ${CONFIG_FILE}..."
        $SUDO install -m 640 "${TMP_DIR}/config" "$CONFIG_FILE"
        $SUDO chown root:root "$CONFIG_FILE"
    fi
else
    info "Config file already exists at ${CONFIG_FILE}, skipping..."
fi

# =============================================================================
# Install service
# =============================================================================

if [ "$OS" = "darwin" ]; then
    # macOS: launchd plist
    PLIST_DIR="$HOME/Library/LaunchAgents"
    PLIST_PATH="${PLIST_DIR}/com.kernel.hypeman.plist"
    mkdir -p "$PLIST_DIR"

    info "Installing launchd service..."

    # Build environment variables from config file
    ENV_DICT=""
    if [ -f "$CONFIG_FILE" ]; then
        while IFS= read -r line; do
            # Skip comments and empty lines
            [[ "$line" =~ ^[[:space:]]*# ]] && continue
            [[ -z "$line" ]] && continue
            key="${line%%=*}"
            value="${line#*=}"
            ENV_DICT="${ENV_DICT}
        <key>${key}</key>
        <string>${value}</string>"
        done < "$CONFIG_FILE"
    fi

    cat > "$PLIST_PATH" << PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.kernel.hypeman</string>
    <key>ProgramArguments</key>
    <array>
        <string>${INSTALL_DIR}/${BINARY_NAME}</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/opt/e2fsprogs/sbin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>${ENV_DICT}
    </dict>
    <key>KeepAlive</key>
    <true/>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>${DATA_DIR}/logs/hypeman.log</string>
    <key>StandardErrorPath</key>
    <string>${DATA_DIR}/logs/hypeman.log</string>
</dict>
</plist>
PLIST

    info "Loading ${SERVICE_NAME} service..."
    launchctl load "$PLIST_PATH"
else
    # Linux: systemd
    info "Installing systemd service..."
    $SUDO tee "${SYSTEMD_DIR}/${SERVICE_NAME}.service" > /dev/null << EOF
[Unit]
Description=Hypeman API Server
Documentation=https://github.com/kernel/hypeman
After=network.target

[Service]
Type=simple
Environment="HOME=${DATA_DIR}"
EnvironmentFile=${CONFIG_FILE}
ExecStart=${INSTALL_DIR}/${BINARY_NAME}
Restart=on-failure
RestartSec=5
KillMode=process

# Security hardening
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF

    info "Reloading systemd..."
    $SUDO systemctl daemon-reload

    info "Enabling ${SERVICE_NAME} service..."
    $SUDO systemctl enable "$SERVICE_NAME"

    info "Starting ${SERVICE_NAME} service..."
    $SUDO systemctl start "$SERVICE_NAME"
fi

# =============================================================================
# Build builder image (macOS)
# =============================================================================

if [ "$OS" = "darwin" ]; then
    info "Attempting to build builder image..."
    if command -v docker >/dev/null 2>&1; then
        if [ -n "$BRANCH" ] && [ -d "${TMP_DIR}/hypeman" ]; then
            BUILD_CONTEXT="${TMP_DIR}/hypeman"
        else
            BUILD_CONTEXT=""
        fi

        if [ -n "$BUILD_CONTEXT" ] && [ -f "${BUILD_CONTEXT}/lib/builds/images/generic/Dockerfile" ]; then
            if ! docker build -t hypeman/builder:latest -f "${BUILD_CONTEXT}/lib/builds/images/generic/Dockerfile" "$BUILD_CONTEXT" 2>/dev/null; then
                warn "Failed to build builder image. You can build it later manually."
            else
                info "Builder image built successfully"
            fi
        else
            warn "Builder image Dockerfile not available. Build it manually: docker build -t hypeman/builder:latest -f lib/builds/images/generic/Dockerfile ."
        fi
    else
        warn "Docker not available, skipping builder image build"
    fi
fi

# =============================================================================
# Install Hypeman CLI
# =============================================================================

CLI_REPO="kernel/hypeman-cli"

# CLI releases use goreleaser naming: "macos" not "darwin", .zip not .tar.gz on macOS
if [ "$OS" = "darwin" ]; then
    CLI_OS="macos"
    CLI_EXT="zip"
else
    CLI_OS="$OS"
    CLI_EXT="tar.gz"
fi

if [ -z "$CLI_VERSION" ] || [ "$CLI_VERSION" == "latest" ]; then
    info "Fetching latest CLI version with available artifacts..."
    CLI_VERSION=$(find_release_with_artifact "$CLI_REPO" "hypeman" "$CLI_OS" "$ARCH" "$CLI_EXT" || true)
    if [ -z "$CLI_VERSION" ]; then
        warn "Failed to find a CLI release with artifacts for ${CLI_OS}/${ARCH}, skipping CLI installation"
    fi
fi

if [ -n "$CLI_VERSION" ]; then
    info "Installing Hypeman CLI version: $CLI_VERSION"

    CLI_VERSION_NUM="${CLI_VERSION#v}"
    CLI_ARCHIVE_NAME="hypeman_${CLI_VERSION_NUM}_${CLI_OS}_${ARCH}.${CLI_EXT}"
    CLI_DOWNLOAD_URL="https://github.com/${CLI_REPO}/releases/download/${CLI_VERSION}/${CLI_ARCHIVE_NAME}"

    info "Downloading CLI ${CLI_ARCHIVE_NAME}..."
    if curl -fsSL "$CLI_DOWNLOAD_URL" -o "${TMP_DIR}/${CLI_ARCHIVE_NAME}"; then
        info "Extracting CLI..."
        mkdir -p "${TMP_DIR}/cli"
        if [ "$CLI_EXT" = "zip" ]; then
            unzip -qo "${TMP_DIR}/${CLI_ARCHIVE_NAME}" -d "${TMP_DIR}/cli"
        else
            tar -xzf "${TMP_DIR}/${CLI_ARCHIVE_NAME}" -C "${TMP_DIR}/cli"
        fi

        if [ "$OS" = "darwin" ]; then
            info "Installing hypeman CLI to ${INSTALL_DIR}..."
            $SUDO install -m 755 "${TMP_DIR}/cli/hypeman" "${INSTALL_DIR}/hypeman"
        else
            # Install CLI binary
            info "Installing hypeman CLI to ${INSTALL_DIR}..."
            $SUDO install -m 755 "${TMP_DIR}/cli/hypeman" "${INSTALL_DIR}/hypeman-cli"

            # Install wrapper script to /usr/local/bin for PATH access
            info "Installing hypeman wrapper to /usr/local/bin..."
            $SUDO tee /usr/local/bin/hypeman > /dev/null << WRAPPER
#!/bin/bash
# Wrapper script for hypeman CLI that auto-generates API token
set -a
source ${CONFIG_FILE}
set +a
export HYPEMAN_API_KEY=\$(${INSTALL_DIR}/hypeman-token -user-id "cli-user-\$(whoami)" 2>/dev/null)
exec ${INSTALL_DIR}/hypeman-cli "\$@"
WRAPPER
            $SUDO chmod 755 /usr/local/bin/hypeman
        fi
    else
        warn "Failed to download CLI from ${CLI_DOWNLOAD_URL}, skipping CLI installation"
    fi
fi

# =============================================================================
# Done
# =============================================================================

echo ""
echo -e "${PURPLE}"
cat << 'EOF'
 ██╗  ██╗  ██╗   ██╗  ██████╗   ███████╗  ███╗   ███╗   █████╗   ███╗   ██╗
 ██║  ██║  ╚██╗ ██╔╝  ██╔══██╗  ██╔════╝  ████╗ ████║  ██╔══██╗  ████╗  ██║
 ███████║   ╚████╔╝   ██████╔╝  █████╗    ██╔████╔██║  ███████║  ██╔██╗ ██║
 ██╔══██║    ╚██╔╝    ██╔═══╝   ██╔══╝    ██║╚██╔╝██║  ██╔══██║  ██║╚██╗██║
 ██║  ██║     ██║     ██║       ███████╗  ██║ ╚═╝ ██║  ██║  ██║  ██║ ╚████║
 ╚═╝  ╚═╝     ╚═╝     ╚═╝       ╚══════╝  ╚═╝     ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚═══╝
EOF
echo -e "${NC}"
info "Hypeman installed successfully!"
echo ""

if [ "$OS" = "darwin" ]; then
    echo "  API Binary:   ${INSTALL_DIR}/${BINARY_NAME}"
    echo "  CLI:          ${INSTALL_DIR}/hypeman"
    echo "  Token tool:   ${INSTALL_DIR}/hypeman-token"
    echo "  Config:       ${CONFIG_FILE}"
    echo "  Data:         ${DATA_DIR}"
    echo "  Service:      ~/Library/LaunchAgents/com.kernel.hypeman.plist"
    echo "  Logs:         ${DATA_DIR}/logs/hypeman.log"
else
    echo "  API Binary:   ${INSTALL_DIR}/${BINARY_NAME}"
    echo "  CLI:          /usr/local/bin/hypeman"
    echo "  Token tool:   /usr/local/bin/hypeman-token"
    echo "  Config:       ${CONFIG_FILE}"
    echo "  Data:         ${DATA_DIR}"
    echo "  Service:      ${SERVICE_NAME}.service"
fi

echo ""
echo ""
echo "Next steps:"
echo "  - (Optional) Edit ${CONFIG_FILE} to configure your installation"
echo ""
echo "Get Started:"
echo "╭──────────────────────────────────────────╮"
echo "│  hypeman pull nginx:alpine               │"
echo "│  hypeman run nginx:alpine                │"
echo "│  hypeman logs <instance-id>              │"
echo "│  hypeman exec -it <instance-id> /bin/sh  │"
echo "│  hypeman --help                          │"
echo "╰──────────────────────────────────────────╯"
echo ""
