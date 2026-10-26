package instances

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMacOSHealthCheckAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		policy                     *healthcheck.Policy
		declared, skipped, allowed bool
	}{
		{"exec", &healthcheck.Policy{Type: healthcheck.TypeExec, Exec: &healthcheck.ExecCheck{Command: []string{"/usr/bin/true"}}}, true, false, true},
		{"inferred exec", &healthcheck.Policy{Exec: &healthcheck.ExecCheck{Command: []string{"/usr/bin/true"}}}, true, false, true},
		{"undeclared", &healthcheck.Policy{Type: healthcheck.TypeExec, Exec: &healthcheck.ExecCheck{Command: []string{"/usr/bin/true"}}}, false, false, false},
		{"skipped", &healthcheck.Policy{Type: healthcheck.TypeExec, Exec: &healthcheck.ExecCheck{Command: []string{"/usr/bin/true"}}}, true, true, false},
		{"tcp", &healthcheck.Policy{Type: healthcheck.TypeTCP, TCP: &healthcheck.TCPCheck{Port: 80}}, true, false, false},
		{"http", &healthcheck.Policy{Type: healthcheck.TypeHTTP, HTTP: &healthcheck.HTTPCheck{Port: 80}}, true, false, false},
		{"none", &healthcheck.Policy{Type: healthcheck.TypeNone}, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := testMacImage()
			image.MacOS.GuestAgent = tc.declared
			req := CreateInstanceRequest{HealthCheck: tc.policy, SkipGuestAgent: tc.skipped, NetworkEnabled: true}
			createErr := prepareMacOSCreate(&req, image, testMacOSCaps())
			stored := StoredMetadata{MacOS: image.MacOS, SkipGuestAgent: tc.skipped}
			updateErr := checkMacOSUpdate(&stored, UpdateInstanceRequest{HealthCheck: tc.policy})
			if tc.allowed {
				require.NoError(t, createErr)
				require.NoError(t, updateErr)
			} else {
				require.ErrorIs(t, createErr, ErrInvalidRequest)
				require.ErrorIs(t, updateErr, ErrInvalidRequest)
			}
		})
	}
}

func TestMacOSUpdateHealthCheckPersistsAndDisables(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p, macOSOnly: true, lifecycleEvents: newLifecycleSubscribers()}
	id := "mac-health"
	image := testMacImage()
	image.MacOS.GuestAgent = true
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: id, DataDir: p.InstanceDir(id), MacOS: image.MacOS}}))

	policy := &healthcheck.Policy{Type: healthcheck.TypeExec, Exec: &healthcheck.ExecCheck{Command: []string{"/usr/bin/true"}}}
	updated, err := m.UpdateInstance(context.Background(), id, UpdateInstanceRequest{HealthCheck: policy})
	require.NoError(t, err)
	require.Equal(t, healthcheck.TypeExec, updated.HealthCheck.Type)
	require.NotEmpty(t, updated.HealthCheck.Interval)
	persisted, err := m.loadMetadata(id)
	require.NoError(t, err)
	require.Equal(t, updated.HealthCheck, persisted.HealthCheck)

	updated, err = m.UpdateInstance(context.Background(), id, UpdateInstanceRequest{HealthCheck: &healthcheck.Policy{Type: healthcheck.TypeNone}})
	require.NoError(t, err)
	require.False(t, healthcheck.Enabled(updated.HealthCheck))
	require.Nil(t, updated.HealthCheckRuntime)
}
