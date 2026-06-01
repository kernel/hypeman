# UFFD Snapshot Pager

The UFFD pager lets Firecracker restore snapshot memory lazily. A restored VM
gets a per-session UFFD socket, and page faults are served from the snapshot
memory file by a local pager process.

The pager is local to the host. It is not backed by Redis or another external
cache because page faults are latency-sensitive and the kernel-facing UFFD
socket is local. The process keeps one shared in-memory page cache, bounded by
`hypervisor.firecracker_uffd_cache_max_bytes`. Cache entries are keyed by a
snapshot cache key plus page offset, so multiple restore sessions from the same
snapshot can reuse hot pages without starting one pager per snapshot.

UFFD is opt-in through `hypervisor.firecracker_snapshot_memory_backend=uffd`.
The default backend remains `file`. Enabling UFFD does not change already
running VMs; it only changes future Firecracker snapshot restores. If a restore
uses UFFD, the VM is pinned to the pager session created for that restore until
the instance stops, is deleted, or otherwise closes the session.

Some restore-time memory writes, such as the resume-network mailbox payload,
must not mutate the backing snapshot memory file. Those writes are represented
as overlay pages on the pager session. When the guest faults that page, the
pager serves the overlay for that restore only; other sessions continue to read
the original snapshot page.
