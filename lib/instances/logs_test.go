package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShouldSkipAppLogLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "program start marker",
			line: "2026-03-10T10:00:00Z [INFO] HYPEMAN-PROGRAM-START ts=2026-03-10T10:00:00Z mode=exec",
			want: true,
		},
		{
			name: "agent ready marker",
			line: "2026-03-10T10:00:01Z [INFO] HYPEMAN-AGENT-READY ts=2026-03-10T10:00:01Z",
			want: true,
		},
		{
			name: "headers marker",
			line: "2026-03-10T10:00:02Z [INFO] HYPEMAN-HEADERS-READY",
			want: true,
		},
		{
			name: "normal app log line",
			line: "2026-03-10T10:00:03Z [INFO] build completed successfully",
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, shouldSkipAppLogLine(tt.line))
		})
	}
}
