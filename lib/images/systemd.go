package images

// IsSystemdImage checks if the image's CMD indicates it wants systemd as init.
// Detection is based on the effective command (entrypoint + cmd), not whether
// systemd is installed in the image.
//
// Returns true if the image's command is:
//   - /sbin/init
//   - /lib/systemd/systemd
//   - /usr/lib/systemd/systemd
func IsSystemdImage(entrypoint, cmd []string) bool {
	// Combine to get the actual command that will run.
	// Create a new slice to avoid corrupting caller's backing array.
	effective := make([]string, 0, len(entrypoint)+len(cmd))
	effective = append(effective, entrypoint...)
	effective = append(effective, cmd...)
	if len(effective) == 0 {
		return false
	}

	first := effective[0]

	// Match specific systemd/init paths only.
	// We intentionally don't match generic */init paths since many entrypoint
	// scripts are named "init" and would be false positives.
	systemdPaths := []string{
		"/sbin/init",
		"/lib/systemd/systemd",
		"/usr/lib/systemd/systemd",
	}
	for _, p := range systemdPaths {
		if first == p {
			return true
		}
	}

	return false
}

