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

## Constraints

- Volumes can only be attached at instance creation time (no hot-attach)
- A volume can only be attached to one instance at a time
- Deleting an instance detaches its volumes but does not delete them

## Storage

Volumes are stored as sparse raw disk files at `{dataDir}/volumes/{id}/data.raw`, pre-formatted as ext4. Sparse files only consume actual disk space for written data.

