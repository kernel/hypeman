//go:build linux

package instances

import (
	"os"
	"testing"
)

const standbyRestoreCompressionManualEnv = "HYPEMAN_RUN_STANDBY_RESTORE_COMPRESSION_TESTS"

func requireStandbyRestoreCompressionManualRun(t *testing.T) {
	t.Helper()
	if os.Getenv(standbyRestoreCompressionManualEnv) != "1" {
		t.Skipf("set %s=1 to run standby/restore compression integration tests", standbyRestoreCompressionManualEnv)
	}
}
