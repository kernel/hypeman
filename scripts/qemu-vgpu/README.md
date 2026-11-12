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

Snapshots record the runtime binary and firmware checksums, the machine type, the NVIDIA host-driver version, the PCI vendor and device IDs and the vGPU profile type. Restores reject any mismatch. A fork claims its own VF and gets its own vsock CID and machine UUID; the source's device assignment is never reused. Forks hardlink the template's memory file instead of copying it, and each fork's guest RAM is private and populated up front rather than shared copy-on-write. `hypervisor.vgpu_max_concurrent_restores` (default 2) caps concurrent restores.

Capture uses live VFIO precopy: do not pause the guest before requesting a standby snapshot. The source's VF is released only after migration completes and its VMM exits. A restore loads the stream into a paused guest, resumes it, and has the guest agent apply the fork's network identity; a guest whose agent does not answer within ten seconds fails the restore. QEMU's post-migration self-announcement is disabled for these restores.

A snapshot clones guest state as it is. Capture templates without per-instance secrets, and have a restored guest refresh whatever must be unique per instance (machine identity, entropy, licenses, sessions) before it serves work. Keep a cold-boot path as the fallback, and keep the runtime that existing snapshots were captured with installed until those snapshots are gone.
