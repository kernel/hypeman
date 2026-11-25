package paths

import (
	"strconv"
	"strings"
)

const forkBackupSeparator = ".fork-bak."

// ForkBackupPath is the temporary location of a parked Firecracker source.
func ForkBackupPath(source string, stamp int64) string {
	return source + forkBackupSeparator + strconv.FormatInt(stamp, 10)
}

// ForkBackupSource parses a parked directory name, not its full path.
func ForkBackupSource(name string) (string, bool) {
	id, stamp, found := strings.Cut(name, forkBackupSeparator)
	if !found {
		return "", false
	}
	_, err := strconv.ParseUint(stamp, 10, 63)
	return id, err == nil
}
