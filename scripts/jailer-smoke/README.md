# Jailer test setup

The jailer tests in `lib/hypervisor/firecracker` boot real guests. They need a Linux
host with root and `/dev/kvm`.

    # guest init that prints a counter once a second
    scripts/jailer-smoke/build-rootfs.sh heartbeat.ext4 scripts/jailer-smoke/heartbeat-init.c

    # embedded firecracker and jailer
    make ensure-firecracker-binaries
    # kernel: hypeman's pinned Kernel_202610301 x86_64 vmlinux

    cd lib/hypervisor/firecracker
    sudo env HYPEMAN_TEST_KERNEL=/path/vmlinux \
      HYPEMAN_HEARTBEAT_ROOTFS=$PWD/../../../scripts/jailer-smoke/heartbeat.ext4 \
      go test -tags jailer_integration -count=1 -run 'Jailed|StandbySnapshotRestore' .

Measurements behind the design (deft-kernel-dev, shared host, small samples):

- Jailed and unjailed guests have the same random 4 KiB read latency (about 40 us) and
  IOPS (about 25k). The jail adds nothing per I/O.
- Jailer setup adds about 25-30 ms per VMM start. The binary copy is about 7 ms of that.
- The API socket was polled every 50 ms, which added up to 50 ms to every start. It is
  polled every 2 ms now.
