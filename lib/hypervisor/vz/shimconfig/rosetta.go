package shimconfig

// RosettaMountTag is the virtio-fs tag for the Rosetta directory share. The
// host (vz-shim) attaches the share under this tag and the guest (init) mounts
// it by the same tag, so the value is a shared constant rather than a wire
// field. This file carries no build constraint so the Linux-built guest init
// can reference the tag while the rest of the package stays darwin-only.
const RosettaMountTag = "rosetta"
