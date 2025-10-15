#!/bin/bash

VERSION='ch-release-v6.12.8-20250613'

# https://github.com/cloud-hypervisor/linux/releases
wget https://github.com/cloud-hypervisor/linux/releases/download/$VERSION/vmlinux-x86_64 -O vmlinux
