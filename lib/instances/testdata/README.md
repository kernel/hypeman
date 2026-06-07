# test fixtures

## rosetta_probe_amd64

A tiny freestanding, statically-linked **x86-64** Linux ELF used by
`TestVZRosettaX86Exec` to prove that a vz guest booted with `EnableRosetta`
can execute amd64 binaries via Apple Rosetta. It writes `ROSETTA_X86_OK` to
stdout and exits 0, using only raw `write`/`exit` syscalls (no libc), so it
runs purely through Rosetta's binfmt_misc dispatch without needing any amd64
shared libraries in the guest.

Source: `rosetta_probe_amd64.c`. Rebuild (on an x86-64 host) with:

```
gcc -static -nostdlib -no-pie -fno-asynchronous-unwind-tables -Os -s \
    -o rosetta_probe_amd64 rosetta_probe_amd64.c
```

The binary is committed because the macOS CI runner cannot cross-compile a
Linux amd64 ELF; it is intentionally minimal (~8.8 KB) and reproducible from
the source above.
