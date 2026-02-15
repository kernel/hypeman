#!/bin/bash
#
# Hypeman E2E Install Test
#
# Runs a full install → verify → uninstall cycle.
# Platform-agnostic: works on both Linux and macOS.
#

set -e

# Colors
RED='\033[38;2;255;110;110m'
GREEN='\033[38;2;92;190;83m'
YELLOW='\033[0;33m'
NC='\033[0m'

info() { echo -e "${GREEN}[INFO]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }
pass() { echo -e "${GREEN}[PASS]${NC} $1"; }
fail() { echo -e "${RED}[FAIL]${NC} $1"; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')

cd "$REPO_DIR"

# =============================================================================
# Phase 1: Clean slate
# =============================================================================
info "Phase 1: Cleaning previous installation..."
KEEP_DATA=false bash scripts/uninstall.sh 2>/dev/null || true

# =============================================================================
# Phase 2: Install from source
# =============================================================================
info "Phase 2: Installing from source..."
BRANCH=$(git rev-parse --abbrev-ref HEAD)
# Build CLI from source too when CLI_BRANCH is set (e.g., for testing unreleased CLI features)
BRANCH="$BRANCH" CLI_BRANCH="${CLI_BRANCH:-}" bash scripts/install.sh

# =============================================================================
# Phase 3: Wait for service
# =============================================================================
info "Phase 3: Waiting for service to be healthy..."

PORT=8080
TIMEOUT=60
ELAPSED=0

while [ $ELAPSED -lt $TIMEOUT ]; do
    if curl -sf "http://localhost:${PORT}/health" >/dev/null 2>&1; then
        pass "Service is responding on port ${PORT}"
        break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
done

if [ $ELAPSED -ge $TIMEOUT ]; then
    # Dump logs for debugging
    if [ "$OS" = "darwin" ]; then
        LOG_FILE="$HOME/Library/Application Support/hypeman/logs/hypeman.log"
        if [ -f "$LOG_FILE" ]; then
            warn "Service logs (last 50 lines):"
            tail -50 "$LOG_FILE" || true
        else
            warn "No log file found at $LOG_FILE"
        fi
        warn "launchctl list:"
        launchctl list | grep hypeman || true
    fi
    fail "Service did not become healthy within ${TIMEOUT}s"
fi

# =============================================================================
# Phase 4: Validate installation
# =============================================================================
info "Phase 4: Validating installation..."

# Check binaries
if [ "$OS" = "darwin" ]; then
    [ -x /usr/local/bin/hypeman-api ] || fail "hypeman-api binary not found"
    pass "Binaries installed correctly"

    # Check launchd service
    if launchctl list | grep -q com.kernel.hypeman; then
        pass "launchd service is loaded"
    else
        fail "launchd service not loaded"
    fi
else
    [ -x /opt/hypeman/bin/hypeman-api ] || fail "hypeman-api binary not found"
    pass "Binaries installed correctly"

    # Check systemd service
    if systemctl is-active --quiet hypeman; then
        pass "systemd service is running"
    else
        fail "systemd service not running"
    fi
fi

# Check config files
if [ "$OS" = "darwin" ]; then
    [ -f "$HOME/.config/hypeman/config.yaml" ] || fail "Server config file not found"
else
    [ -f /etc/hypeman/config.yaml ] || fail "Server config file not found"
fi
pass "Server config file exists"

[ -f "$HOME/.config/hypeman/cli.yaml" ] || fail "CLI config file not found"
pass "CLI config file exists"

# =============================================================================
# Phase 4b: Testing CLI commands
# =============================================================================
info "Phase 4b: Testing CLI commands..."

# hypeman-token should be able to find jwt_secret from config.yaml automatically
if [ "$OS" = "darwin" ]; then
    API_KEY=$("/usr/local/bin/hypeman-token" -user-id "e2e-test-user")
else
    API_KEY=$("/usr/local/bin/hypeman-token" -user-id "e2e-test-user")
fi
[ -n "$API_KEY" ] || fail "Failed to generate API token (hypeman-token should find jwt_secret from config.yaml)"
pass "hypeman-token reads jwt_secret from config.yaml"

# Determine CLI path
HYPEMAN_CMD="/usr/local/bin/hypeman"

# Verify CLI was installed
[ -x "$HYPEMAN_CMD" ] || fail "hypeman CLI not found at $HYPEMAN_CMD"
pass "CLI installed"

$HYPEMAN_CMD ps || fail "hypeman ps failed"
pass "hypeman ps works"

# VM lifecycle test
E2E_VM_NAME="e2e-test-vm"

$HYPEMAN_CMD pull nginx:alpine || fail "hypeman pull failed"
pass "hypeman pull works"

# Wait for image to be available (pull is async)
IMAGE_READY=false
for i in $(seq 1 30); do
    if $HYPEMAN_CMD run --name "$E2E_VM_NAME" nginx:alpine 2>&1; then
        IMAGE_READY=true
        break
    fi
    sleep 2
done
[ "$IMAGE_READY" = true ] || fail "hypeman run failed (image not ready after 60s)"
pass "hypeman run works"

# Wait for VM to be ready
VM_READY=false
for i in $(seq 1 30); do
    if $HYPEMAN_CMD exec "$E2E_VM_NAME" -- echo "hello" >/dev/null 2>&1; then
        VM_READY=true
        break
    fi
    sleep 2
done
[ "$VM_READY" = true ] || fail "VM did not become ready within 60s"

OUTPUT=$($HYPEMAN_CMD exec "$E2E_VM_NAME" -- echo "hello from e2e") || fail "hypeman exec failed"
echo "$OUTPUT" | grep -q "hello from e2e" || fail "hypeman exec output mismatch: $OUTPUT"
pass "hypeman exec works"

$HYPEMAN_CMD stop "$E2E_VM_NAME" || fail "hypeman stop failed"
pass "hypeman stop works"

$HYPEMAN_CMD rm "$E2E_VM_NAME" || fail "hypeman rm failed"
pass "hypeman rm works"

# =============================================================================
# Phase 5: Cleanup
# =============================================================================
info "Phase 5: Cleaning up..."
KEEP_DATA=false bash scripts/uninstall.sh

# =============================================================================
# Phase 6: Verify cleanup
# =============================================================================
info "Phase 6: Verifying cleanup..."

if [ "$OS" = "darwin" ]; then
    [ ! -f /usr/local/bin/hypeman-api ] || fail "hypeman-api binary still exists after uninstall"
    if launchctl list 2>/dev/null | grep -q com.kernel.hypeman; then
        fail "launchd service still loaded after uninstall"
    fi
else
    [ ! -f /opt/hypeman/bin/hypeman-api ] || fail "hypeman-api binary still exists after uninstall"
    if systemctl is-active --quiet hypeman 2>/dev/null; then
        fail "systemd service still running after uninstall"
    fi
fi
pass "Cleanup verified"

# =============================================================================
# Done
# =============================================================================
echo ""
info "All E2E install tests passed!"
