package api

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestProbeDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	c := probeDisk(dir)
	require.Equal(t, "disk", c.Name)
	require.True(t, c.Ok, "expected disk probe on a tmpdir to pass: %+v", c)
	require.NotNil(t, c.Detail)
}

func TestProbeDiskMissingPath(t *testing.T) {
	t.Parallel()

	c := probeDisk("/nonexistent/path/that/should/not/resolve")
	require.Equal(t, "disk", c.Name)
	require.False(t, c.Ok)
	require.NotNil(t, c.Error)
}

func TestRunDiagnosticChecksReturnsAllChecks(t *testing.T) {
	t.Parallel()

	checks := runDiagnosticChecks(context.Background(), t.TempDir())
	require.Len(t, checks, 3)

	names := make(map[string]oapi.DiagnosticCheck, len(checks))
	for _, c := range checks {
		names[c.Name] = c
	}
	require.Contains(t, names, "dns")
	require.Contains(t, names, "egress_tcp")
	require.Contains(t, names, "disk")

	// Disk on tmpdir should always succeed.
	require.True(t, names["disk"].Ok)
}
