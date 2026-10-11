package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/require"
)

func TestGuestAgentReadyExecutableMatchesGuestOS(t *testing.T) {
	require.Equal(t, "/bin/true", guestAgentReadyExecutable(&StoredMetadata{}))
	require.Equal(t, "/usr/bin/true", guestAgentReadyExecutable(&StoredMetadata{MacOS: &images.MacOSImage{}}))
}
