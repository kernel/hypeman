#!/bin/bash
#
# Provision the VM with Hypeman dependencies
#
# Run this script ON the VM (after SSHing in), or from your Mac:
#   ssh hypeman 'bash -s' < bootstrap-dev-environment.sh
#

set -e

echo "=== Provisioning Hypeman Development Environment ==="
echo ""

# Update packages
echo "[1/10] Updating packages..."
sudo apt update
sudo apt upgrade -y

# Install dependencies
echo "[2/10] Installing dependencies..."
sudo apt install -y \
    build-essential \
    git \
    curl \
    wget \
    make \
    erofs-utils \
    dnsmasq \
    libcap2-bin \
    cpu-checker

# Install GitHub CLI
echo "[3/10] Installing GitHub CLI..."
if command -v gh &>/dev/null; then
    echo "GitHub CLI already installed: $(gh --version | head -1)"
else
    sudo mkdir -p -m 755 /etc/apt/keyrings
    out=$(mktemp)
    wget -nv -O"$out" https://cli.github.com/packages/githubcli-archive-keyring.gpg
    cat "$out" | sudo tee /etc/apt/keyrings/githubcli-archive-keyring.gpg > /dev/null
    sudo chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
    sudo mkdir -p -m 755 /etc/apt/sources.list.d
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null
    sudo apt update
    sudo apt install gh -y
    rm -f "$out"
    echo "GitHub CLI installed: $(gh --version | head -1)"
fi

# Install Go
echo "[4/10] Installing Go..."
if go version 2>/dev/null | grep -q "go1.2"; then
    echo "Go already installed: $(go version)"
else
    GO_VERSION="1.25.4"
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz" | sudo tar -C /usr/local -xzf -
    echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
    export PATH=$PATH:/usr/local/go/bin
    echo "Go ${GO_VERSION} installed"
fi

# Configure passwordless sudo
echo "[5/10] Configuring passwordless sudo..."
SUDOERS_FILE="/etc/sudoers.d/$(whoami)-nopasswd"
if [ -f "$SUDOERS_FILE" ]; then
    echo "Passwordless sudo already configured"
else
    echo "$(whoami) ALL=(ALL) NOPASSWD:ALL" | sudo tee "$SUDOERS_FILE" > /dev/null
    sudo chmod 440 "$SUDOERS_FILE"
    echo "Passwordless sudo configured for $(whoami)"
fi

# Enable IP forwarding
echo "[6/10] Enabling IP forwarding..."
sudo sysctl -w net.ipv4.ip_forward=1
grep -q 'net.ipv4.ip_forward=1' /etc/sysctl.conf || echo 'net.ipv4.ip_forward=1' | sudo tee -a /etc/sysctl.conf

# Configure KVM access
echo "[7/10] Configuring KVM..."
sudo chmod 666 /dev/kvm 2>/dev/null || true
sudo usermod -aG kvm "$(whoami)" 2>/dev/null || true

# Create Hypeman data directory
echo "[8/10] Creating Hypeman data directory..."
sudo mkdir -p /var/lib/hypeman
sudo chown "$(whoami):$(whoami)" /var/lib/hypeman
echo "Created /var/lib/hypeman (owned by $(whoami))"

# Verify KVM
echo "[9/10] Verifying KVM..."
if kvm-ok 2>&1 | grep -q "can be used"; then
    echo "✓ KVM acceleration is available"
else
    echo "⚠ KVM may not be available (nested virtualization requires M3+ and macOS 15+)"
fi

# Clone onkernel repositories
echo "[10/10] Cloning onkernel repositories..."
mkdir -p ~/code

REPOS=(
    "onkernel/hypeman"
    "onkernel/hypeman-ts"
    "onkernel/hypeman-go"
    "onkernel/hypeman-cli"
    "onkernel/linux"
)

# Check GitHub authentication status
if gh auth status 2>&1 | grep -q "You are not logged into any GitHub hosts"; then
    echo "  GitHub CLI not authenticated, logging in..."
    gh auth login
fi

for repo in "${REPOS[@]}"; do
    repo_name="${repo#*/}"
    target_dir=~/code/"$repo_name"
    if [ -d "$target_dir" ]; then
        echo "  $repo_name already exists, skipping..."
    else
        echo "  Cloning $repo..."
        gh repo clone "$repo" "$target_dir"
    fi
done

echo ""
echo "=== Provisioning Complete ==="
echo ""
echo "Go version: $(go version 2>/dev/null || echo 'reload shell with: source ~/.bashrc')"
echo "GitHub CLI: $(gh --version 2>/dev/null | head -1 || echo 'installed')"
echo ""
echo "Repositories cloned to ~/code/:"
ls -1 ~/code/ 2>/dev/null | sed 's/^/  - /'
echo ""
echo "Next steps:"
echo "  1. cd ~/code/hypeman && make dev"
echo ""
