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
# Determine version to install
# =============================================================================

if [ -z "$VERSION" ]; then
    info "Fetching latest version..."
    VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)
    if [ -z "$VERSION" ]; then
        error "Failed to fetch latest version"
    fi
fi
info "Installing version: $VERSION"

# =============================================================================
# Download and extract
# =============================================================================

# Construct download URL
VERSION_NUM="${VERSION#v}"
ARCHIVE_NAME="hypeman_${VERSION_NUM}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE_NAME}"

# Create temp directory
TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR" EXIT

info "Downloading ${ARCHIVE_NAME}..."
if ! curl -fsSL "$DOWNLOAD_URL" -o "${TMP_DIR}/${ARCHIVE_NAME}"; then
    error "Failed to download from ${DOWNLOAD_URL}"
fi

info "Extracting..."
tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "$TMP_DIR"

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
# Create directories
# =============================================================================

info "Creating data directory at ${DATA_DIR}..."
$SUDO mkdir -p "$DATA_DIR"

info "Creating config directory at ${CONFIG_DIR}..."
$SUDO mkdir -p "$CONFIG_DIR"

# =============================================================================
# Create config file (if it doesn't exist)
# =============================================================================

if [ ! -f "$CONFIG_FILE" ]; then
    info "Downloading config template..."
    CONFIG_URL="https://raw.githubusercontent.com/${REPO}/${VERSION}/.env.example"
    if ! curl -fsSL "$CONFIG_URL" -o "${TMP_DIR}/config"; then
        error "Failed to download config template from ${CONFIG_URL}"
    fi
    
    # Generate random JWT secret
    info "Generating JWT secret..."
    JWT_SECRET=$(openssl rand -hex 32)
    sed -i "s/^JWT_SECRET=$/JWT_SECRET=${JWT_SECRET}/" "${TMP_DIR}/config"
    
    info "Installing config file at ${CONFIG_FILE}..."
    $SUDO install -m 600 "${TMP_DIR}/config" "$CONFIG_FILE"
    
    # Give ownership to the installing user so they can read the config for CLI
    INSTALL_USER="${SUDO_USER:-$(whoami)}"
    $SUDO chown "$INSTALL_USER" "$CONFIG_FILE"
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
EnvironmentFile=${CONFIG_FILE}
ExecStart=${INSTALL_DIR}/${BINARY_NAME}
Restart=on-failure
RestartSec=5

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=${DATA_DIR}

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
