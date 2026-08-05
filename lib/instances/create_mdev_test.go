package instances

import (
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/stretchr/testify/assert"
)

func TestWrapCreateMdevErr(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name               string
		err                error
		wantMessage        string
		wantInvalidRequest bool
	}{
		{
			name:               "macOS vGPU unsupported",
			err:                devices.ErrVGPUNotSupportedOnMacOS,
			wantMessage:        "invalid request: vGPU (mdev) is not supported on macOS",
			wantInvalidRequest: true,
		},
		{
			name:        "other mdev error",
			err:         errors.New("boom"),
			wantMessage: "create vGPU mdev for profile profile: boom",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := wrapCreateMdevErr("profile", tc.err)

			assert.ErrorIs(t, err, tc.err)
			if tc.wantInvalidRequest {
				assert.ErrorIs(t, err, ErrInvalidRequest)
			} else {
				assert.NotErrorIs(t, err, ErrInvalidRequest)
			}
			assert.Equal(t, tc.wantMessage, err.Error())
		})
	}
}
