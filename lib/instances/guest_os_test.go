package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/require"
)

func TestGuestOperationCapabilityTable(t *testing.T) {
	linux := &StoredMetadata{}
	for _, op := range []GuestOperation{GuestOpExec, GuestOpFileCopy, GuestOpStat} {
		require.Empty(t, linux.GuestOperationUnsupportedReason(op), "Linux guests keep their historical behavior")
	}

	withoutAgent := &StoredMetadata{MacOS: &images.MacOSImage{GuestAgent: false}, SkipGuestAgent: true}
	require.Contains(t, withoutAgent.GuestOperationUnsupportedReason(GuestOpExec), "requires the shared guest agent")

	withAgent := &StoredMetadata{MacOS: &images.MacOSImage{GuestAgent: true}}
	for _, op := range []GuestOperation{GuestOpExec, GuestOpFileCopy, GuestOpStat} {
		require.Empty(t, withAgent.GuestOperationUnsupportedReason(op))
	}
}
