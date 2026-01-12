#!/bin/bash
#
# Hypeman Uninstall Script
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/kernel/hypeman/main/scripts/uninstall.sh | bash
#
# Options (via environment variables):
#   KEEP_DATA=false   - Remove data directory (/var/lib/hypeman) - kept by default
#   KEEP_CONFIG=true  - Keep config directory (/etc/hypeman)
#

set -e

INSTALL_DIR="/opt/hypeman"
DATA_DIR="/var/lib/hypeman"
CONFIG_DIR="/etc/hypeman"
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
# Pre-flight checks
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
        if ! sudo -v < /dev/tty; then
            error "Failed to obtain sudo privileges"
        fi
    fi
    SUDO="sudo"
fi

# =============================================================================
# Stop and disable service
# =============================================================================

if $SUDO systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
    info "Stopping ${SERVICE_NAME} service..."
    $SUDO systemctl stop "$SERVICE_NAME"
fi

if $SUDO systemctl is-enabled --quiet "$SERVICE_NAME" 2>/dev/null; then
    info "Disabling ${SERVICE_NAME} service..."
    $SUDO systemctl disable "$SERVICE_NAME"
fi

# =============================================================================
# Remove systemd service
# =============================================================================

if [ -f "${SYSTEMD_DIR}/${SERVICE_NAME}.service" ]; then
    info "Removing systemd service..."
    $SUDO rm -f "${SYSTEMD_DIR}/${SERVICE_NAME}.service"
    $SUDO systemctl daemon-reload
fi

# =============================================================================
# Remove binaries and wrappers
# =============================================================================

info "Removing binaries..."

# Remove wrapper scripts from /usr/local/bin
$SUDO rm -f /usr/local/bin/hypeman
$SUDO rm -f /usr/local/bin/hypeman-token

# Remove install directory
if [ -d "$INSTALL_DIR" ]; then
    $SUDO rm -rf "$INSTALL_DIR"
fi

# =============================================================================
# Handle data directory
# =============================================================================

if [ -d "$DATA_DIR" ]; then
    if [ "${KEEP_DATA:-true}" = "true" ]; then
        info "Keeping data directory: ${DATA_DIR}"
    else
        info "Removing data directory: ${DATA_DIR}"
        $SUDO rm -rf "$DATA_DIR"
    fi
fi

# =============================================================================
# Handle config directory
# =============================================================================

if [ -d "$CONFIG_DIR" ]; then
    if [ "${KEEP_CONFIG:-false}" = "true" ]; then
        warn "Keeping config directory: ${CONFIG_DIR}"
    else
        info "Removing config directory: ${CONFIG_DIR}"
        $SUDO rm -rf "$CONFIG_DIR"
    fi
fi

# =============================================================================
# Remove hypeman user
# =============================================================================

if id "$SERVICE_USER" &>/dev/null; then
    if [ "${KEEP_DATA:-true}" = "true" ]; then
        info "Keeping system user: ${SERVICE_USER} (data is preserved)"
    else
        info "Removing system user: ${SERVICE_USER}"
        $SUDO userdel "$SERVICE_USER" 2>/dev/null || true
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
info "Hypeman uninstalled successfully!"
echo ""

if [ "${KEEP_DATA:-true}" = "true" ] && [ -d "$DATA_DIR" ]; then
    info "Data directory preserved: ${DATA_DIR}"
    echo "  To remove: sudo rm -rf ${DATA_DIR}"
    echo ""
fi

if [ "${KEEP_CONFIG:-false}" = "true" ] && [ -d "$CONFIG_DIR" ]; then
    info "Config directory preserved: ${CONFIG_DIR}"
    echo "  To remove: sudo rm -rf ${CONFIG_DIR}"
    echo ""
fi

warn "Note: Caddy or Cloud Hypervisor processes may still be running."
echo "  Check with: ps aux | grep -E 'caddy|cloud-h'"
echo "  Kill all:   sudo pkill -f caddy; sudo pkill -f cloud-h"
echo ""

echo "To reinstall:"
echo "  curl -fsSL https://raw.githubusercontent.com/kernel/hypeman/main/scripts/install.sh | bash"
echo ""
