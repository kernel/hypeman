# Cloud Hypervisor POC

Proof of concept for running 10 Chromium VMs simultaneously using Cloud Hypervisor with disk-based overlays, config disks, networking isolation, and standby/restore functionality.

## Prerequisites

Install cloud-hypervisor by [installing the pre-built binaries](https://www.cloudhypervisor.org/docs/prologue/quick-start/#use-pre-built-binaries). Make sure `ch-remote` and `cloud-hypervisor` are in path.

```bash
ch-remote --version
cloud-hypervisor --version
```

Tested with version `v48.0.0`

Note: Requires `kernel-images-private` cloned to home directory with `iproute2` installed in the Chromium headful image.

## Setup

Build kernel, initrd, and rootfs with config disk support:

```bash
./scripts/build-initrd.sh
```

This creates:
- `data/system/vmlinux` - Linux kernel
- `data/system/initrd` - BusyBox init with disk-based overlay
- `data/images/chromium-headful/v1/rootfs.ext4` - Chromium rootfs (read-only, shared)

Configure host network with bridge and guest isolation:

```bash
./scripts/setup-host-network.sh
```

Create 10 VM configurations (IPs 192.168.100.10-19, isolated TAP devices, overlay disks, config disks):

```bash
./scripts/setup-vms.sh
```

## Running VMs

Start all 10 VMs:

```bash
./scripts/start-all-vms.sh
```

Check VM status:

```bash
./scripts/list-vms.sh
```

View VM logs:

```bash
./scripts/logs-vm.sh <vm-id>     # Show last 100 lines
./scripts/logs-vm.sh <vm-id> -f  # Follow logs
```

SSH into a VM:

```bash
./scripts/ssh-vm.sh <vm-id>      # Password: root
```

Stop a VM:

```bash
./scripts/stop-vm.sh <vm-id>
./scripts/stop-all-vms.sh        # Stop all
```

## Standby / Restore

Standby a VM (pause, snapshot, delete VMM):

```bash
./scripts/standby-vm.sh <vm-id>
```

Restore a VM from snapshot:

```bash
./scripts/restore-vm.sh <vm-id>
```

## Networking

Enable port forwarding for WebRTC access (localhost:8080-8089 → guest VMs):

```bash
./scripts/setup-port-forwarding.sh
```

Connect to a VM:

```bash
./scripts/connect-guest.sh <vm-id>
```

## Volumes

Create a persistent volume:

```bash
./scripts/create-volume.sh <vol-id> <size-gb>
```

## Architecture

- **Disk-based overlay**: Each VM has a 50GB sparse overlay disk on `/dev/vdb` (faster restore than tmpfs)
- **Config disk**: Each VM has a config disk on `/dev/vdc` with VM-specific settings (IP, MAC, envs)
- **Guest isolation**: VMs cannot communicate with each other (iptables + bridge_slave isolation)
- **Serial logging**: All VM output captured to `data/guests/guest-N/logs/console.log`
- **Shared rootfs**: Single read-only rootfs image shared across all VMs
