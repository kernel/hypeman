#!/bin/bash
#
# Hypeman API Install Script
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/onkernel/hypeman/main/scripts/install.sh | bash
#
# Options (via environment variables):
#   VERSION      - Install specific API version (default: latest)
#   CLI_VERSION  - Install specific CLI version (default: latest)
#   BRANCH       - Build from source using this branch (for development/testing)
#   INSTALL_DIR  - Binary installation directory (default: /opt/hypeman/bin)
#   DATA_DIR     - Data directory (default: /var/lib/hypeman)
#   CONFIG_DIR   - Config directory (default: /etc/hypeman)
#

set -e

REPO="onkernel/hypeman"
BINARY_NAME="hypeman-api"
INSTALL_DIR="${INSTALL_DIR:-/opt/hypeman/bin}"
DATA_DIR="${DATA_DIR:-/var/lib/hypeman}"
CONFIG_DIR="${CONFIG_DIR:-/etc/hypeman}"
CONFIG_FILE="${CONFIG_DIR}/config"
SYSTEMD_DIR="/etc/systemd/system"
SERVICE_NAME="hypeman"
SERVICE_USER="hypeman"

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

info "Running pre-flight checks..."

# Check for root or sudo access
SUDO=""
if [ "$EUID" -ne 0 ]; then
    if ! command -v sudo >/dev/null 2>&1; then
        error "This script requires root privileges. Please run as root or install sudo."
    fi
    # Try passwordless sudo first, then prompt from terminal if needed
    if ! sudo -n true 2>/dev/null; then
        info "Requesting sudo privileges..."
        # Read password from /dev/tty (terminal) even when script is piped
        if ! sudo -v < /dev/tty; then
            error "Failed to obtain sudo privileges"
        fi
    fi
    SUDO="sudo"
fi

# Check for required commands
command -v curl >/dev/null 2>&1 || error "curl is required but not installed"
command -v tar >/dev/null 2>&1 || error "tar is required but not installed"
command -v systemctl >/dev/null 2>&1 || error "systemctl is required but not installed (systemd not available?)"
command -v setcap >/dev/null 2>&1 || error "setcap is required but not installed (install libcap2-bin)"
command -v openssl >/dev/null 2>&1 || error "openssl is required but not installed"

# Additional checks for build-from-source mode
if [ -n "$BRANCH" ]; then
    command -v git >/dev/null 2>&1 || error "git is required for BRANCH mode but not installed"
    command -v go >/dev/null 2>&1 || error "go is required for BRANCH mode but not installed"
    command -v make >/dev/null 2>&1 || error "make is required for BRANCH mode but not installed"
fi

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
if [ "$OS" != "linux" ]; then
    error "Hypeman only supports Linux (detected: $OS)"
fi

# Detect architecture
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

info "Pre-flight checks passed"

# =============================================================================
# Create temp directory
# =============================================================================

TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR" EXIT

# =============================================================================
# Get binaries (either download release or build from source)
# =============================================================================

if [ -n "$BRANCH" ]; then
    # Build from source mode
    info "Building from source (branch: $BRANCH)..."
    
    BUILD_DIR="${TMP_DIR}/hypeman"
    git clone --branch "$BRANCH" --depth 1 "https://github.com/${REPO}.git" "$BUILD_DIR"
    
    info "Building binaries (this may take a few minutes)..."
    cd "$BUILD_DIR"
    
    # Build main binary (includes dependencies)
    make build
    cp "bin/hypeman" "${TMP_DIR}/${BINARY_NAME}"
    
    # Build hypeman-token (not included in make build)
    info "Building hypeman-token..."
    go build -o "${TMP_DIR}/hypeman-token" ./cmd/gen-jwt
    
    # Copy .env.example for config template
    cp ".env.example" "${TMP_DIR}/.env.example"
    
    VERSION="$BRANCH (source)"
    cd - > /dev/null
else
    # Download release mode
    if [ -z "$VERSION" ]; then
        info "Fetching latest version..."
        VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)
        if [ -z "$VERSION" ]; then
            error "Failed to fetch latest version"
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
fi

# =============================================================================
# Stop existing service if running
# =============================================================================

if $SUDO systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
    info "Stopping existing ${SERVICE_NAME} service..."
    $SUDO systemctl stop "$SERVICE_NAME"
fi

# =============================================================================
# Install binaries
# =============================================================================

info "Installing ${BINARY_NAME} to ${INSTALL_DIR}..."
$SUDO mkdir -p "$INSTALL_DIR"
$SUDO install -m 755 "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"

# Set capabilities for network operations
info "Setting capabilities..."
$SUDO setcap 'cap_net_admin,cap_net_bind_service=+eip' "${INSTALL_DIR}/${BINARY_NAME}"

# Install hypeman-token binary
info "Installing hypeman-token to ${INSTALL_DIR}..."
$SUDO install -m 755 "${TMP_DIR}/hypeman-token" "${INSTALL_DIR}/hypeman-token"

# Install wrapper script to /usr/local/bin for easy access
info "Installing hypeman-token wrapper to /usr/local/bin..."
$SUDO tee /usr/local/bin/hypeman-token > /dev/null << EOF
#!/bin/bash
# Wrapper script for hypeman-token that loads config from /etc/hypeman/config
set -a
source ${CONFIG_FILE}
set +a
exec ${INSTALL_DIR}/hypeman-token "\$@"
EOF
$SUDO chmod 755 /usr/local/bin/hypeman-token

# =============================================================================
# Create hypeman system user
# =============================================================================

if ! id "$SERVICE_USER" &>/dev/null; then
    info "Creating system user: ${SERVICE_USER}..."
    $SUDO useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

# Add hypeman user to kvm group for VM access
if getent group kvm &>/dev/null; then
    $SUDO usermod -aG kvm "$SERVICE_USER"
fi

# =============================================================================
# Create directories
# =============================================================================

info "Creating data directory at ${DATA_DIR}..."
$SUDO mkdir -p "$DATA_DIR"
$SUDO chown "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR"

info "Creating config directory at ${CONFIG_DIR}..."
$SUDO mkdir -p "$CONFIG_DIR"

# =============================================================================
# Create config file (if it doesn't exist)
# =============================================================================

if [ ! -f "$CONFIG_FILE" ]; then
    # Get config template (from local build or download from repo)
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
    # Replace entire line to avoid trailing comments being included in the value
    sed -i "s/^# CADDY_ADMIN_PORT=.*/CADDY_ADMIN_PORT=2019/" "${TMP_DIR}/config"
    sed -i "s/^# INTERNAL_DNS_PORT=.*/INTERNAL_DNS_PORT=5353/" "${TMP_DIR}/config"
    
    info "Installing config file at ${CONFIG_FILE}..."
    $SUDO install -m 640 "${TMP_DIR}/config" "$CONFIG_FILE"
    
    # Set ownership: installing user owns the file, hypeman group can read it
    # This allows CLI (running as user) and service (running as hypeman) to both read
    INSTALL_USER="${SUDO_USER:-$(whoami)}"
    $SUDO chown "${INSTALL_USER}:${SERVICE_USER}" "$CONFIG_FILE"
else
    info "Config file already exists at ${CONFIG_FILE}, skipping..."
fi

# =============================================================================
# Install systemd service
# =============================================================================

info "Installing systemd service..."
$SUDO tee "${SYSTEMD_DIR}/${SERVICE_NAME}.service" > /dev/null << EOF
[Unit]
Description=Hypeman API Server
Documentation=https://github.com/onkernel/hypeman
After=network.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
Environment="HOME=${DATA_DIR}"
EnvironmentFile=${CONFIG_FILE}
ExecStart=${INSTALL_DIR}/${BINARY_NAME}
Restart=on-failure
RestartSec=5

# Security hardening
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=${DATA_DIR}
# Note: NoNewPrivileges=true is omitted because we need capabilities

# Capabilities for network operations
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF

# Reload systemd
info "Reloading systemd..."
$SUDO systemctl daemon-reload

# Enable service
info "Enabling ${SERVICE_NAME} service..."
$SUDO systemctl enable "$SERVICE_NAME"

# Start service
info "Starting ${SERVICE_NAME} service..."
$SUDO systemctl start "$SERVICE_NAME"

# =============================================================================
# Install Hypeman CLI
# =============================================================================

CLI_REPO="onkernel/hypeman-cli"

if [ -z "$CLI_VERSION" ]; then
    info "Fetching latest CLI version..."
    CLI_VERSION=$(curl -fsSL "https://api.github.com/repos/${CLI_REPO}/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)
    if [ -z "$CLI_VERSION" ]; then
        warn "Failed to fetch latest CLI version, skipping CLI installation"
        CLI_VERSION=""
    fi
fi

if [ -n "$CLI_VERSION" ]; then
    info "Installing Hypeman CLI version: $CLI_VERSION"
    
    CLI_VERSION_NUM="${CLI_VERSION#v}"
    CLI_ARCHIVE_NAME="hypeman_${CLI_VERSION_NUM}_${OS}_${ARCH}.tar.gz"
    CLI_DOWNLOAD_URL="https://github.com/${CLI_REPO}/releases/download/${CLI_VERSION}/${CLI_ARCHIVE_NAME}"
    
    info "Downloading CLI ${CLI_ARCHIVE_NAME}..."
    if curl -fsSL "$CLI_DOWNLOAD_URL" -o "${TMP_DIR}/${CLI_ARCHIVE_NAME}"; then
        info "Extracting CLI..."
        mkdir -p "${TMP_DIR}/cli"
        tar -xzf "${TMP_DIR}/${CLI_ARCHIVE_NAME}" -C "${TMP_DIR}/cli"
        
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
echo "  API Binary:   ${INSTALL_DIR}/${BINARY_NAME}"
echo "  CLI:          /usr/local/bin/hypeman"
echo "  Token tool:   /usr/local/bin/hypeman-token"
echo "  Config:       ${CONFIG_FILE}"
echo "  Data:         ${DATA_DIR}"
echo "  Service:      ${SERVICE_NAME}.service"
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
echo "│  hypeman --help                          │"
echo "╰──────────────────────────────────────────╯"
echo ""
