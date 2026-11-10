# Opt-in QEMU vGPU snapshot runtime

This package builds QEMU 11.1.3 from a pinned source commit with the bounded VFIO state-load batching patch. It does not replace the system QEMU. Linux amd64 vendor-VFIO L40S-3Q is the initial supported configuration.

Build on Ubuntu 24.04 (glibc 2.39), or use the container build so binaries also run on Ubuntu 24.04 hosts:

```sh
docker build --output type=local,dest=./qemu-vgpu-runtime \
  -f scripts/qemu-vgpu/Dockerfile .
```

The `Build QEMU vGPU runtime` workflow builds pull-request artifacts. A manual run from main with `publish=true` creates a new `qemu-vgpu-<version>` release with the runtime archive, corresponding patched source archive, and `SHA256SUMS`; it does not replace an existing release or trigger the API's `v*` release workflow. Install only an explicitly pinned archive checksum.

The build runs the patched load routine against partial writes, EINTR, truncated reads, write errors, zero writes and multi-buffer payloads under ASan/UBSan. `build.sh OUTPUT_DIRECTORY` is also available on a compatible build host with the Dockerfile's dependencies installed. Output directories must not already exist.

Install the resulting directory, preserving its manifest and firmware, at:

```text
<data_dir>/system/binaries/qemu/11.1.3-vgpu.1/x86_64/
```

Select it explicitly on a new instance with `hypervisor: qemu`, `hypervisor_version: 11.1.3-vgpu.1`, `gpu.profile: NVIDIA L40S-3Q`, and the guest agent enabled. Ordinary QEMU instances keep using the system binary. Never overwrite an installed runtime that has live snapshots: change the runtime version and rebuild templates instead.

Snapshots pin the runtime binary and BIOS checksums, machine type, NVIDIA host-driver version and PCI device identity. Restores reject mismatches. A child claims its own VF and receives a separate vsock CID; the captured source device assignment is not reused. RAM is eagerly allocated privately per child, not shared through CPU-style copy-on-write. `hypervisor.vgpu_max_concurrent_restores` defaults to two.

Capture uses live VFIO precopy: do not pause the guest before requesting a standby snapshot. Only after migration completes and the source VMM exits is its VF released. Restores load the mapped-RAM stream paused before guest network reconfiguration and application identity handoff.

The runtime does not make arbitrary application state safe to clone. Browser templates must contain no customer state, license token, trusted license cache or active lease. The companion browser image keeps licensing deferred until the child has its own MAC/license identity, mixes fresh host entropy, and restarts Chromium before returning readiness. Licensing then proceeds asynchronously with normal health checks. Host/runtime support must deploy before enabling GPU fanout in the browser control plane.

Keep GPU fanout disabled until the complete template/identity/licensing flow, concurrent forks, cancellation, failed restore cleanup, old-template behavior and neighboring workload impact have been qualified. Retain the existing cold-boot path for fallback and the previous runtime for existing snapshots.
