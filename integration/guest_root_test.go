package integration

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/stretchr/testify/require"
)

// assertGuestRootIsOverlay checks that the guest's / is the image overlay
// rather than the initrd, i.e. init switched root instead of only chrooting.
func assertGuestRootIsOverlay(t *testing.T, ctx context.Context, inst *instances.Instance) {
	t.Helper()
	out, exitCode, err := execInInstance(ctx, inst, "cat", "/proc/self/mountinfo")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, out)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] != "/" {
			continue
		}
		sep := slices.Index(fields, "-")
		require.True(t, sep > 0 && sep+1 < len(fields), "malformed mountinfo line: %s", line)
		require.Equal(t, "overlay", fields[sep+1], "guest / is not the image overlay: %s", line)
		return
	}
	t.Fatalf("no root mount in mountinfo:\n%s", out)
}
