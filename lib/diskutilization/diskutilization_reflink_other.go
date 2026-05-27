//go:build !linux

package diskutilization

type sharedExtentTracker struct{}

func newSharedExtentTracker() *sharedExtentTracker {
	return &sharedExtentTracker{}
}

func snapshotAllocatedBytesForPath(path string, _ *sharedExtentTracker) (int64, int64) {
	return allocatedBytesForPath(path), 0
}
