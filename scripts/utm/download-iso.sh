#!/bin/bash
#
# Download Ubuntu Server ARM64 ISO
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGES_DIR="${SCRIPT_DIR}/images"

UBUNTU_VERSION="24.04.3"
UBUNTU_ISO_URL="https://cdimage.ubuntu.com/releases/24.04/release/ubuntu-${UBUNTU_VERSION}-live-server-arm64.iso"
UBUNTU_ISO="${IMAGES_DIR}/ubuntu-${UBUNTU_VERSION}-live-server-arm64.iso"

mkdir -p "${IMAGES_DIR}"

if [[ -f "${UBUNTU_ISO}" ]]; then
    echo "ISO already exists: ${UBUNTU_ISO}"
    echo "Size: $(du -h "${UBUNTU_ISO}" | cut -f1)"
    exit 0
fi

echo "Downloading Ubuntu Server ${UBUNTU_VERSION} ARM64..."
echo "URL: ${UBUNTU_ISO_URL}"
echo "This will take a few minutes (~3GB)..."
echo ""

curl -L -o "${UBUNTU_ISO}" --progress-bar "${UBUNTU_ISO_URL}"

echo ""
echo "Download complete!"
echo "ISO: ${UBUNTU_ISO}"
echo "Size: $(du -h "${UBUNTU_ISO}" | cut -f1)"
echo ""
echo "Next: Create the VM in UTM following the instructions in README.md"



