package api

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInstanceCtx returns a context with a fake instance in the given state,
// simulating the ResolveResource middleware. No real instance is needed.
func fakeInstanceCtx(id string, state instances.State) context.Context {
	inst := &instances.Instance{}
	inst.Id = id
	inst.State = state
	return mw.WithResolvedInstance(ctx(), id, inst)
}

func TestWaitForInstanceState_AlreadyInTargetState(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.WaitForInstanceState(
		fakeInstanceCtx("test-already", instances.StateStopped),
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-already",
			Params: oapi.WaitForInstanceStateParams{
				State:   oapi.InstanceStateStopped,
				Timeout: lo.ToPtr("5s"),
			},
		},
	)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	assert.Equal(t, oapi.InstanceStateStopped, result.State)
	assert.False(t, result.TimedOut)
}

func TestWaitForInstanceState_AlreadyRunning(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.WaitForInstanceState(
		fakeInstanceCtx("test-running", instances.StateRunning),
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-running",
			Params: oapi.WaitForInstanceStateParams{
				State:   oapi.InstanceStateRunning,
				Timeout: lo.ToPtr("5s"),
			},
		},
	)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	assert.Equal(t, oapi.InstanceStateRunning, result.State)
	assert.False(t, result.TimedOut)
}

func TestWaitForInstanceState_TerminalStateEarlyReturn(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// Stopped is terminal — waiting for Running should return immediately.
	resp, err := svc.WaitForInstanceState(
		fakeInstanceCtx("test-terminal", instances.StateStopped),
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-terminal",
			Params: oapi.WaitForInstanceStateParams{
				State:   oapi.InstanceStateRunning,
				Timeout: lo.ToPtr("30s"),
			},
		},
	)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	assert.Equal(t, oapi.InstanceStateStopped, result.State)
	assert.False(t, result.TimedOut)
}

func TestWaitForInstanceState_StandbyTerminalEarlyReturn(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// Standby is terminal — waiting for Running should return immediately.
	resp, err := svc.WaitForInstanceState(
		fakeInstanceCtx("test-standby-terminal", instances.StateStandby),
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-standby-terminal",
			Params: oapi.WaitForInstanceStateParams{
				State:   oapi.InstanceStateRunning,
				Timeout: lo.ToPtr("30s"),
			},
		},
	)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	assert.Equal(t, oapi.InstanceStateStandby, result.State)
	assert.False(t, result.TimedOut)
}

func TestWaitForInstanceState_InvalidTimeout(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.WaitForInstanceState(
		fakeInstanceCtx("test-invalid-to", instances.StateStopped),
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-invalid-to",
			Params: oapi.WaitForInstanceStateParams{
				State:   oapi.InstanceStateRunning,
				Timeout: lo.ToPtr("not-a-duration"),
			},
		},
	)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState400JSONResponse)
	require.True(t, ok, "expected 400 response, got %T", resp)
	assert.Equal(t, "invalid_timeout", result.Code)
}

func TestWaitForInstanceState_NegativeTimeout(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.WaitForInstanceState(
		fakeInstanceCtx("test-neg-to", instances.StateStopped),
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-neg-to",
			Params: oapi.WaitForInstanceStateParams{
				State:   oapi.InstanceStateRunning,
				Timeout: lo.ToPtr("-5s"),
			},
		},
	)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState400JSONResponse)
	require.True(t, ok, "expected 400 response, got %T", resp)
	assert.Equal(t, "invalid_timeout", result.Code)
}

func TestWaitForInstanceState_NoResolvedInstance(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.WaitForInstanceState(
		ctx(),
		oapi.WaitForInstanceStateRequestObject{
			Id: "nonexistent",
			Params: oapi.WaitForInstanceStateParams{
				State: oapi.InstanceStateRunning,
			},
		},
	)
	require.NoError(t, err)

	_, ok := resp.(oapi.WaitForInstanceState500JSONResponse)
	require.True(t, ok, "expected 500 response, got %T", resp)
}

func TestWaitForInstanceState_InstanceDeletedDuringWait(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// Fake context with Initializing state (non-terminal, non-target)
	// Instance doesn't exist in manager → poll returns ErrNotFound → 404
	start := time.Now()
	resp, err := svc.WaitForInstanceState(
		fakeInstanceCtx("test-deleted", instances.StateInitializing),
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-deleted",
			Params: oapi.WaitForInstanceStateParams{
				State:   oapi.InstanceStateRunning,
				Timeout: lo.ToPtr("30s"),
			},
		},
	)
	elapsed := time.Since(start)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState404JSONResponse)
	require.True(t, ok, "expected 404 response, got %T", resp)
	assert.Equal(t, "not_found", result.Code)
	assert.Less(t, elapsed, 10*time.Second, "should detect deletion within first poll")
}

func TestWaitForInstanceState_ContextCancellation(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	cancelCtx, cancel := context.WithCancel(fakeInstanceCtx("test-cancel", instances.StateInitializing))

	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	resp, err := svc.WaitForInstanceState(
		cancelCtx,
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-cancel",
			Params: oapi.WaitForInstanceStateParams{
				State:   oapi.InstanceStateRunning,
				Timeout: lo.ToPtr("30s"),
			},
		},
	)
	elapsed := time.Since(start)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	assert.True(t, result.TimedOut, "should report timed_out on context cancellation")
	assert.Less(t, elapsed, 5*time.Second, "should return soon after context cancellation")
}

func TestWaitForInstanceState_TimeoutCapped(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// Request 1h timeout with Stopped instance waiting for Stopped → already in target state
	resp, err := svc.WaitForInstanceState(
		fakeInstanceCtx("test-capped", instances.StateStopped),
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-capped",
			Params: oapi.WaitForInstanceStateParams{
				State:   oapi.InstanceStateStopped,
				Timeout: lo.ToPtr("1h"),
			},
		},
	)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	assert.Equal(t, oapi.InstanceStateStopped, result.State)
	assert.False(t, result.TimedOut)
}

func TestWaitForInstanceState_DefaultTimeout(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// No timeout specified — already in target state so returns immediately
	resp, err := svc.WaitForInstanceState(
		fakeInstanceCtx("test-defto", instances.StateStandby),
		oapi.WaitForInstanceStateRequestObject{
			Id: "test-defto",
			Params: oapi.WaitForInstanceStateParams{
				State: oapi.InstanceStateStandby,
			},
		},
	)
	require.NoError(t, err)

	result, ok := resp.(oapi.WaitForInstanceState200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	assert.Equal(t, oapi.InstanceStateStandby, result.State)
	assert.False(t, result.TimedOut)
}
