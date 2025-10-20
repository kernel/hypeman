#!/bin/bash

VM=vm1
TAP="tap-$VM"
MAC="52:55:00:d1:55:01"    # pick unique per VM
GUEST_IP="192.168.100.10"  # unique per VM in the /24
MASK="255.255.255.0"

# create tap (idempotent-ish)
ip link show "$TAP" &>/dev/null || sudo ip tuntap add "$TAP" mode tap user "$(whoami)"
sudo ip link set "$TAP" up
sudo ip link set "$TAP" master vmbr0

sudo rm /tmp/ch.sock || true
sudo cloud-hypervisor \
  --kernel vmlinux \
  --initramfs initrd \
  --cmdline 'console=hvc0' \
  --cpus boot=4 --memory size=8192M \
  --net "tap=${TAP},ip=${GUEST_IP},mask=${MASK},mac=${MAC}" \
  --api-socket /tmp/ch.sock

  # TODO: add config or stateful chrome volumes?
  # --disk path=cfg.img,readonly=on \
  # NOTE: 8192M memory - something different about how initrd unpack happens in unikraft,
  # allowing for less memory
