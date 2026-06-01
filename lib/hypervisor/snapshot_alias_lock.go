package hypervisor

import "sync"

var snapshotSourceAliasMu sync.RWMutex

// LockSnapshotSourceAliasMutation serializes the short Firecracker restore
// window where a snapshotted source guest directory is temporarily replaced by
// a symlink to the fork being restored.
func LockSnapshotSourceAliasMutation() func() {
	snapshotSourceAliasMu.Lock()
	return snapshotSourceAliasMu.Unlock
}

// LockSnapshotSourceAliasReaders blocks source-directory readers while a
// snapshot source alias mutation is active.
func LockSnapshotSourceAliasReaders() func() {
	snapshotSourceAliasMu.RLock()
	return snapshotSourceAliasMu.RUnlock
}
