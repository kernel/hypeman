# mailbox

The resume network handoff uses a small pre-armed mailbox in guest memory to avoid making the first post-resume operation a host-initiated guest RPC. When a Firecracker guest boots with mailbox env enabled, the guest agent allocates and locks a fixed-size buffer containing a magic marker and token. That buffer is captured in standby snapshots.

Before restore, the host finds the marker in the snapshot memory file, writes a JSON network payload into the buffer, and flips the sequence field. After resume, VMGenID wakes the guest watcher, which reads the payload, applies the new network identity locally, and sends a UDP applied ack to the host. If the mailbox is missing, cannot be patched, or does not ack in time, restore falls back to the host-initiated network reconfigure path.
