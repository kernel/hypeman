package integration

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/stretchr/testify/assert"
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

// waitForKernelHeadersReady waits for the async headers install and checks
// the headers and build symlink landed in the image.
func waitForKernelHeadersReady(t *testing.T, ctx context.Context, inst *instances.Instance) {
	t.Helper()
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		out, exitCode, err := execInInstance(ctx, inst, "cat", "/run/hypeman/kernel-headers.status")
		require.NoError(collect, err)
		require.Equal(collect, 0, exitCode, out)
		require.Equal(collect, "ready", strings.TrimSpace(out))
	}, 3*time.Minute, 2*time.Second)

	out, exitCode, err := execInInstance(ctx, inst, "sh", "-c",
		`k=$(uname -r); test -d "/usr/src/linux-headers-$k" && readlink "/lib/modules/$k/build"`)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, out)
	require.True(t, strings.HasPrefix(strings.TrimSpace(out), "/usr/src/linux-headers-"), out)
}
