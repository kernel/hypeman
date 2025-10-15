#!/bin/bash

sudo cloud-hypervisor \
  --kernel vmlinux \
  --initramfs initrd \
  --cmdline 'console=hvc0' \
  --cpus boot=4 --memory size=8192M \
  --api-socket /tmp/ch.sock

  # TODO: add config or stateful chrome volumes?
  # --disk path=cfg.img,readonly=on \
  # NOTE: 8192M memory - something different about how initrd unpack happens in unikraft,
  # allowing for less memory
