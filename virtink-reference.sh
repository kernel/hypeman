#!/bin/bash

# This is how the virink project starts a VM

# 1. Start cloud-hypervisor daemon
cloud-hypervisor --api-socket /var/run/virtink/ch.sock &

# 2. Prepare the rootfs disk (this is what the init container does)
mkdir -p /mnt/ubuntu
truncate -s 4294967296 /mnt/ubuntu/rootfs.raw
mkfs.ext4 -d /path/to/ubuntu/rootfs /mnt/ubuntu/rootfs.raw

# 3. Create the VM config JSON (vm-config.json)
cat > vm-config.json <<EOF
{
  "cpus": {
    "boot_vcpus": 2,
    "max_vcpus": 2
  },
  "memory": {
    "size": 1073741824
  },
  "payload": {
    "kernel": "/path/to/vmlinux",
    "cmdline": "console=ttyS0 root=/dev/vda rw"
  },
  "disks": [
    {
      "id": "rootfs",
      "path": "/mnt/ubuntu/rootfs.raw",
      "direct": true
    }
  ],
  "console": {
    "mode": "Pty"
  },
  "serial": {
    "mode": "Tty"
  }
}
EOF

# 4. Create the VM
curl -X PUT \
  --unix-socket /var/run/virtink/ch.sock \
  http://localhost/api/v1/vm.create \
  -H "Content-Type: application/json" \
  -d @vm-config.json

# 5. Boot the VM
curl -X PUT \
  --unix-socket /var/run/virtink/ch.sock \
  http://localhost/api/v1/vm.boot

# 6. Connect to serial console (optional)
screen /dev/pts/X  # where X is the PTY number from cloud-hypervisor output
