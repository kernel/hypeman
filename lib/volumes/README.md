# Volumes

Volumes are persistent block storage that exist independently of instances. They allow data to survive instance deletion and be reattached to new instances.

## Lifecycle

1. **Create** - `POST /volumes` creates an ext4-formatted sparse disk file of the specified size
2. **Attach** - Specify volumes in `CreateInstanceRequest.volumes` with a mount path
3. **Use** - Volume appears as a block device inside the guest, mounted at the specified path
4. **Detach** - Volumes are automatically detached when an instance is deleted
5. **Delete** - `DELETE /volumes/{id}` removes the volume (fails if still attached)

## Cloud Hypervisor Integration

When an instance with volumes is created, each volume's raw disk file is passed to Cloud Hypervisor as an additional `DiskConfig` entry in the VM configuration. The disks appear in order as `/dev/vdX` devices:

- `/dev/vda` - rootfs (read-only image)
- `/dev/vdb` - overlay (writable instance storage)
- `/dev/vdc` - config disk (read-only)
- `/dev/vdd`, `/dev/vde`, ... - attached volumes

The init process inside the guest reads the requested mount paths from the config disk and mounts each volume at its specified path.

## Multi-Attach (Read-Only Sharing)

A single volume can be attached to multiple instances simultaneously if **all** attachments are read-only. This enables sharing static content (libraries, datasets, config files) across many VMs without duplication.

**Rules:**
- First attachment can be read-write or read-only
- If any attachment is read-write, no other attachments are allowed
- If all existing attachments are read-only, additional read-only attachments are allowed
- Cannot add read-write attachment to a volume with existing attachments

## Overlay Mode

When attaching a volume with `overlay: true`, the instance gets copy-on-write semantics:
- Base volume is mounted read-only (shared)
- A per-instance overlay disk captures all writes
- Instance sees combined view: base data + its local changes
- Other instances don't see each other's overlay writes (isolated)

This allows multiple instances to share a common base (e.g., dataset, model weights) while each can make local modifications without affecting others. Requires `readonly: true` and `overlay_size` specifying the max size of per-instance writes.

## Constraints

- Volumes can only be attached at instance creation time (no hot-attach)
- Deleting an instance detaches its volumes but does not delete them
- Cannot delete a volume while it has any attachments

## Storage

Volumes are stored as sparse raw disk files at `{dataDir}/volumes/{id}/data.raw`, pre-formatted as ext4. Sparse files only consume actual disk space for written data.

