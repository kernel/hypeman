//go:build !linux

package instances

func inspectSnapshotMemorySharing(string) (snapshotMemorySharing, error) {
	return snapshotMemorySharing{Unknown: true}, nil
}
