package instances

import (
	"context"
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/stretchr/testify/assert"
)

type recordingResourceValidator struct {
	reserveCalls int
}

func (v *recordingResourceValidator) ValidateAllocation(context.Context, int, int64, int64, int64, int64, int64, bool) error {
	return nil
}

func (v *recordingResourceValidator) ReserveAllocation(context.Context, string, int, int64, int64, int64, int64, int64, bool) error {
	v.reserveCalls++
	return nil
}

func (v *recordingResourceValidator) FinishAllocation(string) {}

func TestCreateInstanceRejectsUnsupportedVGPUBeforeResourceReservation(t *testing.T) {
	t.Parallel()

	if devices.Capabilities().SupportsVGPU {
		t.Skip("host platform supports vGPU")
	}

	validator := &recordingResourceValidator{}
	m := &manager{resourceValidator: validator}

	_, err := m.createInstance(context.Background(), CreateInstanceRequest{
		Name:  "instance",
		Image: "image",
		GPU:   &GPUConfig{Profile: "profile"},
	})

	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.ErrorIs(t, err, devices.ErrVGPUNotSupportedOnMacOS)
	assert.Zero(t, validator.reserveCalls)
}

func TestWrapCreateVGPUErr(t *testing.T) {
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
			name:        "other vGPU error",
			err:         errors.New("boom"),
			wantMessage: "create vGPU mdev for profile profile: boom",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := wrapCreateVGPUErr("profile", tc.err)

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
