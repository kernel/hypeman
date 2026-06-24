package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveStopTimeout(t *testing.T) {
	intPtr := func(v int) *int { return &v }

	tests := []struct {
		name     string
		stored   int
		override *int
		want     int
	}{
		{name: "unset uses default", stored: 0, override: nil, want: DefaultStopTimeout},
		{name: "negative stored uses default", stored: -3, override: nil, want: DefaultStopTimeout},
		{name: "configured value", stored: 12, override: nil, want: 12},
		{name: "override wins over configured", stored: 12, override: intPtr(30), want: 30},
		{name: "override wins over default", stored: 0, override: intPtr(30), want: 30},
		{name: "non-positive override ignored", stored: 12, override: intPtr(0), want: 12},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveStopTimeout(&StoredMetadata{StopTimeout: tt.stored}, tt.override)
			assert.Equal(t, tt.want, got)
		})
	}
}
