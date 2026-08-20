package devices

// withVGPUPlacementLock runs f. Vendor VFIO placement does not exist on
// darwin, so there is no placement lock to hold.
func withVGPUPlacementLock(f func()) {
	f()
}
