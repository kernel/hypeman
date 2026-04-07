package instances

func volumeOverlayReservationBytes(volumes []VolumeAttachment) int64 {
	var total int64
	for _, vol := range volumes {
		if vol.Overlay {
			total += vol.OverlaySize
		}
	}
	return total
}

func requestedDiskReservationBytes(overlaySize int64, volumes []VolumeAttachment) int64 {
	return overlaySize + volumeOverlayReservationBytes(volumes)
}

func storedDiskReservationBytes(stored *StoredMetadata) int64 {
	if stored == nil {
		return 0
	}
	return requestedDiskReservationBytes(stored.OverlaySize, stored.Volumes)
}
