# Cloud Hypervisor POC

Check that we can run Chrome in a Cloud Hypervisor VM using our existing image builds. Investigate the performance and behavior of Cloud Hypervisor.

## Initial setup:

Install cloud-hypervisor by [installing the pre-built binaries](https://www.cloudhypervisor.org/docs/prologue/quick-start/#use-pre-built-binaries). Make sure `ch-root` and `cloud-hypervisor` are in path (requires renaming the binaries).

```
# this should work
ch-remote --version
cloud-hypervisor --version
```

I used version `v48.0.0`

Download a kernel; we will use this when we start the VM:

```bash
./get-kernel.sh 
```

This will result in the kernel at `./vmlinux`.

## Build initial ram disk and Chromium root filesystem

To minimize the amount of memory required, which speeds up the snapshot/restore process, the boot process works with three main flags.

```
# shown to explain, not to run
...
  --kernel vmlinux \
  --initramfs initrd \
  --disk path=rootfs.ext4,readonly=on \
...
```

We provide the kernel because Docker filesystems don't include kernels, so this is analogous to Docker providing a kernel separately from the root filesystem.

In the Unikraft world, we pack the Chromium root filesystem into the initial ram disk. In this setup, we instead build a BusyBox root filesystem with an initialization script in our initial ram disk. The init script here sets up an in-memory overlay filesystem on top of the attached, read-only Chromium rootfs disk. Then, we switch root into the overlay filesystem. This avoids keeping the entire Chromium filesystem in memory, and Chrome starts up in these VMs with only 2GB of memory (it might work with less; we didn’t try that).

To build the initial ram disk and Chromium root filesystem, run this (check notes below first):

```bash
./build-initrd.sh
```

Note: this assumes you have `kernel-images-private` cloned to your home directory.
Note: you also have to install `iproute2` in the Chromium headful image, see this diff:

```
-    wget ca-certificates python2 supervisor xclip xdotool \
+    wget ca-certificates python2 supervisor xclip xdotool iproute2 \
```

When that is done, you will have `initrd` and `rootfs.ext4`.

In this current setup, configuration (envs) is built directly into the init script, but in a real setup, we would probably mount another disk for this so that the initrd and rootfs stay the same for all VMs on the same container version.

## Run Chromium

First, configure the host’s network to provide a tap interface for the guest VM.

```bash
# this is idempotent
./setup-host-network.sh
```

Note: more investigation is needed before using this networking setup in production.

Next, start Chromium! Note that you will want another shell after this step, so start a tmux session or similar before running this.

```bash
./start-chrome.sh
```

## Perform a snapshot

While Chrome is running in the VM in another shell, take a snapshot:

```bash
./snapshot.sh
```

Now that you have a snapshot, you can kill the VM like this:

```bash
sudo pkill cloud-hyper
```

## Perform a restore

Back in your original shell where you were running the Chrome VM, start a hypervisor without booting a VM:

```bash
./prepare-restore.sh
```

This script just starts up Cloud Hypervisor without any settings, so it’s ready to receive the restore.

Then, perform the restore from your second shell:

```bash
./remote-restore.sh
```

Note: the way this works is by sending an HTTP request to the socket. We don’t have to use the `ch-remote` command-line tool; we can call the API directly, as done in the **virtink** project. See the sample code in `virtink-reference.sh`. This file shows an example of how the virtink project takes a Docker image and boots it into a Cloud Hypervisor VM.

## Next steps

* Can we connect to WebRTC?
* Can we install `sshd` and SSH into the instance?
* Can we connect to CDP and do something?
* Does standby/restore work without interrupting the CDP connection?
