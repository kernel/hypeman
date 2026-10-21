package testsupport

import (
	"runtime"
	"strings"
)

// DeleteBridge removes a test bridge. Its addresses and routes go with it, and
// SweepOrphanedRules removes its iptables rules once the bridge is gone. A
// bridge that no longer exists is not an error; other failures are logged.
func DeleteBridge(name string) {
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := run(nil, "ip", "link", "delete", name, "type", "bridge"); err != nil && !strings.Contains(err.Error(), "Cannot find device") {
		logErr("delete bridge "+name, err)
	}
}
