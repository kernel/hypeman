package builds

import (
	"context"
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/stretchr/testify/require"
)

type abortInstances struct {
	instances.Manager
	state       instances.State
	forcedState instances.State
	forced      bool
	forcedErr   error
}

func (a *abortInstances) GetInstance(context.Context, string) (*instances.Instance, error) {
	return &instances.Instance{State: a.state}, nil
}

func (a *abortInstances) StopInstance(context.Context, string) (*instances.Instance, error) {
	a.forced = true
	if a.forcedErr != nil {
		return nil, a.forcedErr
	}
	if a.forcedState != "" {
		a.state = a.forcedState
	}
	return &instances.Instance{State: a.state}, nil
}

type abortExporter struct {
	receipt instances.MacOSBuildExportReceipt
	err     error
}

func (e abortExporter) StopMacOSBuildInstance(context.Context, string) (instances.MacOSBuildExportReceipt, error) {
	return e.receipt, e.err
}

func (abortExporter) ExportStoppedMacOSBuildInstance(context.Context, string, string) error {
	return errors.New("abort must not export")
}

func newAbortSession(m *abortInstances, e abortExporter) *guestMachineSession {
	return &guestMachineSession{manager: m, exporter: e, instance: &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "builder"}}}
}

func TestMachineAbortUsesStrictStopBeforeForcedFallback(t *testing.T) {
	m := &abortInstances{state: instances.StateRunning}
	s := newAbortSession(m, abortExporter{receipt: instances.MacOSBuildExportReceipt{Graceful: true, VMMExited: true}})
	receipt, err := s.Abort(context.Background())
	require.NoError(t, err)
	require.True(t, receipt.VMMExited)
	require.True(t, receipt.Graceful)
	require.False(t, m.forced, "a confirmed graceful exit must not force-stop")
}

func TestMachineAbortForcedFallbackConfirmsVMMExit(t *testing.T) {
	m := &abortInstances{state: instances.StateStopped}
	s := newAbortSession(m, abortExporter{err: errors.New("agent never became ready")})
	s.instance = &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "builder"}}
	receipt, err := s.Abort(context.Background())
	require.NoError(t, err)
	require.True(t, receipt.VMMExited)
	require.False(t, receipt.Graceful)
}

func TestMachineAbortForcesRunningInstanceToStopped(t *testing.T) {
	m := &abortInstances{state: instances.StateRunning, forcedState: instances.StateStopped}
	s := newAbortSession(m, abortExporter{err: errors.New("agent never became ready")})
	receipt, err := s.Abort(context.Background())
	require.NoError(t, err)
	require.Equal(t, MachineStopReceipt{VMMExited: true}, receipt)
	require.True(t, m.forced)
}

func TestMachineAbortRejectsUnconfirmedForcedStop(t *testing.T) {
	m := &abortInstances{state: instances.StateRunning}
	s := newAbortSession(m, abortExporter{err: errors.New("agent never became ready")})
	receipt, err := s.Abort(context.Background())
	require.EqualError(t, err, "forced stop did not confirm VMM exit")
	require.False(t, receipt.VMMExited)
	require.True(t, m.forced)
}

func TestMachineAbortWithoutConfirmedExitStaysQuarantined(t *testing.T) {
	m := &abortInstances{state: instances.StateRunning, forcedErr: errors.New("hypervisor still alive")}
	s := newAbortSession(m, abortExporter{err: errors.New("agent never became ready")})
	receipt, err := s.Abort(context.Background())
	require.Error(t, err)
	require.False(t, receipt.VMMExited)
	require.Error(t, s.Destroy(context.Background()), "Destroy must refuse without confirmed VMM exit")
}
