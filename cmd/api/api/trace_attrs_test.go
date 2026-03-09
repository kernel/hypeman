package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCpSpanAttributes(t *testing.T) {
	attrs := cpSpanAttributes("inst-123", "to")
	got := map[string]any{}
	for _, attr := range attrs {
		got[string(attr.Key)] = attr.Value.AsInterface()
	}

	require.Equal(t, "inst-123", got["instance_id"])
	require.Equal(t, "to", got["direction"])
	require.NotContains(t, got, "guest_path")
	require.NotContains(t, got, "subject")
}

func TestExecSpanAttributes(t *testing.T) {
	attrs := execSpanAttributes("inst-456", true)
	got := map[string]any{}
	for _, attr := range attrs {
		got[string(attr.Key)] = attr.Value.AsInterface()
	}

	require.Equal(t, "inst-456", got["instance_id"])
	require.Equal(t, true, got["tty"])
	require.NotContains(t, got, "guest_path")
	require.NotContains(t, got, "subject")
}
