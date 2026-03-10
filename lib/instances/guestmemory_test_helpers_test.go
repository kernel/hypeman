package instances

import (
	"os"
	"testing"
)

const guestMemoryManualEnv = "HYPEMAN_RUN_GUESTMEMORY_TESTS"

func requireGuestMemoryManualRun(t *testing.T) {
	t.Helper()
	if os.Getenv(guestMemoryManualEnv) != "1" {
		t.Skipf("set %s=1 to run guest memory integration tests", guestMemoryManualEnv)
	}
}
