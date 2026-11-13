# Experimental VZ live forks

```sh
hypeman fork --from-running --target-state Running source copy
```

This suspends the source, clones its disk and saved machine state, and resumes
both guests. The fork preserves running processes and RAM rather than silently
cold-booting a disk-only copy.

## Platform warning

VZ saved-state restoration requires the original `VZGenericMachineIdentifier`.
Consequently, a restored Linux source and its memory-preserving fork share that
identifier. Apple's `VZGenericPlatformConfiguration.h` states that running two
VMs concurrently with the same identifier results in undefined behavior in the
guest operating system.

Hypeman retains this experimental capability rather than rejecting the fork or
discarding saved RAM. Passing smoke tests does not establish platform support,
cryptographic safety, or reliability across macOS updates. Prefer disposable
workloads until the behavior is validated for the relevant host and guest stack.

For an explicit disk-only copy with a fresh platform identifier:

```sh
hypeman fork --from-running --target-state Stopped source copy
hypeman start copy
```

## Automated guest-state checks

On an Apple Silicon Mac with macOS 14+, a logged-in desktop session, Go, and
Homebrew's `e2fsprogs`:

```sh
make build-embedded sign-vz-shim
PATH="/opt/homebrew/opt/e2fsprogs/sbin:$PATH" \
  go test -tags containers_image_openpgp \
  -run '^TestVZForkFromRunningCorrectness$' -count=3 -v \
  -timeout 10m ./lib/instances
```

The test is part of the normal macOS suite. It checks:

- boot ID, workload PID, tmpfs nonce, and an increasing shell counter survive;
- both guests continue executing, with independent RAM and disk writes;
- fresh guest UUID output differs, as a smoke check rather than a randomness audit;
- the fork survives two additional standby/restore cycles while the source runs.

`TestPrepareForkPreservesMachineIdentifier` separately verifies that fork
preparation retains the identifier required for saved-state compatibility.
Duplicate identifiers alone are not treated as an operational failure.

## Optional network diagnostic

The workload also serves HTTP. Enable post-fork host-to-guest HTTP checks with:

```sh
PATH="/opt/homebrew/opt/e2fsprogs/sbin:$PATH" \
  HYPEMAN_VZ_FORK_HTTP=1 go test -tags containers_image_openpgp \
  -run '^TestVZForkFromRunningCorrectness$' -v -timeout 10m ./lib/instances
```

This diagnostic is opt-in because forked VZ NAT reachability is not yet reliable.
A fork can serve HTTP over loopback and execute over vsock while the host cannot
reach its allocated IP. The guest-state assertions do not imply working network
connectivity, and this symptom has not been attributed to the shared platform ID.

## macOS 27 boot compatibility

The unmodified Cloud Hypervisor-derived kernel can fail before any fork on
macOS 27: its `trap_to_vmm` boot-timing MMIO write can crash the VZ service.
See [apple/container#2360](https://github.com/apple/container/issues/2360).
A failure before the source boots is not evidence of a live-fork identity bug.
Keep results using an unmodified kernel separate from experiments that remove
that hook; passing with a lab kernel does not establish stock-kernel support.
