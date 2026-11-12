# Jailer test setup

The jailer tests in `lib/hypervisor/firecracker` boot real guests. They need a Linux
host with root and `/dev/kvm`.

    # guest init that prints a counter every 50 ms
    scripts/jailer-smoke/build-rootfs.sh heartbeat.ext4 scripts/jailer-smoke/heartbeat-init.c

    make ensure-firecracker-binaries
    # kernel: hypeman's pinned Kernel_202610301 x86_64 vmlinux

    cd lib/hypervisor/firecracker
    sudo env HYPEMAN_TEST_KERNEL=/path/vmlinux \
      HYPEMAN_HEARTBEAT_ROOTFS=$PWD/../../../scripts/jailer-smoke/heartbeat.ext4 \
      go test -tags jailer_integration -count=1 -run 'Jailed|StandbySnapshotRestore' .

Measured on deft-kernel-dev: the jail adds no per-I/O cost (4 KiB random reads, about
40 us and 25k IOPS either way) and about 25-30 ms of setup per VMM start.
