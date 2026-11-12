package network

import "sync"

// TAP devices are created with an owner uid/gid. The default is the hypeman
// process. A jailed firecracker runs as the jailer uid and can only attach to a
// TAP that uid owns, so the jailer config sets the owner through SetTAPOwner.
var (
	tapOwnerMu  sync.RWMutex
	tapOwnerSet bool
	tapOwnerUID int
	tapOwnerGID int
)

// SetTAPOwner sets the uid/gid that owns TAP devices created from now on.
func SetTAPOwner(uid, gid int) {
	tapOwnerMu.Lock()
	defer tapOwnerMu.Unlock()
	tapOwnerSet = true
	tapOwnerUID = uid
	tapOwnerGID = gid
}

// tapOwner returns the configured TAP owner, or the given default when none is set.
func tapOwner(defaultUID, defaultGID int) (int, int) {
	tapOwnerMu.RLock()
	defer tapOwnerMu.RUnlock()
	if !tapOwnerSet {
		return defaultUID, defaultGID
	}
	return tapOwnerUID, tapOwnerGID
}
