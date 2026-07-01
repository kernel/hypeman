# UFFD Graduation

This controller detaches running VMs from the UFFD snapshot memory pager after
they have been up for a while, so long-lived VMs stop depending on a pager (or
on the snapshot backing their restore) and old pager versions can drain to zero
and exit.

## Why detach instead of migrate or fall back to file

A UFFD-backed VM is pinned to its pager session for the life of the restore. The
memory backend (anonymous + userfaultfd vs. a private file mapping) is fixed when
the VM is restored, so there is no way to move a running VM onto the file backend
without restarting the VMM — which would drop its network connections.

What can be done without touching the VM is to let the pager finish its job: it
populates every page that has not yet been faulted in from the backing file, then
unregisters userfaultfd and closes the session. The guest never pauses and its
network is untouched. The VM ends up running on resident memory with no pager
dependency.

The cost is that the populated pages become resident anonymous memory (reclaimable
only to swap, unlike clean file-backed pages), and completion reads the whole
image once (already-resident pages are re-read from disk; only the copy is skipped).
That is why graduation is paced and only applied to VMs that have already had a
soak period.

## What it does

On each scan the controller lists running VMs that still depend on a detachable
pager, then graduates eligible ones subject to the configured limits:

- a session must be at least `min_session_age` old (tracked in memory; a control
  plane restart restarts the soak, which is only more conservative), so
  `min_session_age` is effectively the maximum time a VM spends on the pager
- at most `max_concurrent` graduations run at once
- sessions bound to an outdated pager version are graduated first, so old pager
  versions retire quickly

## Limits

- The feature is disabled by default and only does anything when the host runs the
  `uffd` snapshot memory backend.
- A failed graduation leaves the VM untouched (still on its pager); it is retried
  after a fixed backoff, since every attempt re-reads the whole image.
