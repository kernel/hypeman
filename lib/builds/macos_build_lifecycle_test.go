package builds

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

type machineLifecycleInstances struct {
	instances.Manager
	lookedUp            string
	state, stoppedState instances.State
	missing, stopped    bool
	deleted             bool
	stopErr, deleteErr  error
}

func (m *machineLifecycleInstances) GetInstance(_ context.Context, id string) (*instances.Instance, error) {
	m.lookedUp = id
	if m.missing {
		return nil, instances.ErrNotFound
	}
	return &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "prior"}, State: m.state}, nil
}
func (m *machineLifecycleInstances) StopInstance(context.Context, string) (*instances.Instance, error) {
	m.stopped = true
	if m.stopErr != nil {
		return nil, m.stopErr
	}
	if m.stoppedState != "" {
		m.state = m.stoppedState
	}
	return &instances.Instance{State: m.state}, nil
}
func (m *machineLifecycleInstances) DeleteInstance(context.Context, string) error {
	m.deleted = true
	return m.deleteErr
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

func TestMachineForcedStopCleanup(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		state, stoppedState            instances.State
		graceful, forceError, wantExit bool
	}{
		{"graceful exit", instances.StateRunning, "", true, false, true},
		{"already stopped", instances.StateStopped, "", false, false, true},
		{"force running VM", instances.StateRunning, instances.StateStopped, false, false, true},
		{"unconfirmed stop", instances.StateRunning, "", false, false, false},
		{"failed force stop", instances.StateRunning, "", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &machineLifecycleInstances{state: tc.state, stoppedState: tc.stoppedState}
			stop := MachineStopReceipt{Graceful: tc.graceful, VMMExited: tc.graceful}
			var stopErr error
			if !tc.graceful {
				stopErr = errors.New("agent never became ready")
			}
			if tc.forceError {
				m.stopErr = errors.New("hypervisor still alive")
			}
			s := &guestMachineSession{manager: m, exporter: abortExporter{receipt: stop, err: stopErr}, instance: &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "builder"}}}
			receipt, err := s.Stop(context.Background(), true)
			if tc.wantExit {
				require.NoError(t, err)
				require.Equal(t, MachineStopReceipt{Graceful: tc.graceful, VMMExited: true}, receipt)
			} else {
				wantErr := "forced stop did not confirm VMM exit"
				if tc.forceError {
					wantErr = "hypervisor still alive"
				}
				require.EqualError(t, err, wantErr)
				require.False(t, receipt.VMMExited)
				require.Error(t, s.Destroy(context.Background()), "unconfirmed exit must remain quarantined")
			}
			require.Equal(t, !tc.graceful && tc.state != instances.StateStopped, m.stopped)
		})
	}
}

func TestReapPriorMachineBuilder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      instances.State
		missing    bool
		unrecorded bool
		deleteErr  error
		wantStop   bool
		wantDelete bool
		wantError  bool
	}{
		{"stop and delete", instances.StateRunning, false, false, nil, true, true, false},
		{"delete failure blocks retry", instances.StateStopped, false, false, errors.New("storage still open"), false, true, true},
		{"missing builder", "", true, false, nil, false, false, false},
		{"find unrecorded builder", instances.StateRunning, false, true, nil, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := paths.New(t.TempDir())
			meta := &buildMetadata{ID: "job", Status: StatusQueued}
			if !tc.unrecorded {
				prior := "prior"
				meta.BuilderInstance = &prior
			}
			require.NoError(t, writeMetadata(p, meta))
			fake := &machineLifecycleInstances{state: tc.state, missing: tc.missing, deleteErr: tc.deleteErr}
			m := &manager{paths: p, instanceManager: fake, logger: slog.Default()}
			err := m.reapPriorMachineBuilder(context.Background(), "job")
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.unrecorded, fake.lookedUp == machineBuilderName("job"))
			require.Equal(t, tc.wantStop, fake.stopped)
			require.Equal(t, tc.wantDelete, fake.deleted)
		})
	}
}
