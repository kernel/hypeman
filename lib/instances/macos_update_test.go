package instances

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/paths"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/stretchr/testify/require"
)

func TestMacOSUpdateExpirationAndRestartPolicy(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	p := paths.New(t.TempDir())
	m := &manager{paths: p, now: func() time.Time { return now }, macOSOnly: true, lifecycleEvents: newLifecycleSubscribers()}
	id := "mac-update"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: id, DataDir: p.InstanceDir(id), MacOS: testMacImage().MacOS}}))

	ttl := time.Hour
	updated, err := m.UpdateInstance(context.Background(), id, UpdateInstanceRequest{TTL: &ttl, RestartPolicySet: true, RestartPolicy: &restartpolicy.Policy{Policy: restartpolicy.PolicyAlways}})
	require.NoError(t, err)
	require.Equal(t, now.Add(ttl), *updated.ExpiresAt)
	require.Equal(t, restartpolicy.PolicyAlways, updated.RestartPolicy.Policy)

	expires := now.Add(2 * time.Hour)
	updated, err = m.UpdateInstance(context.Background(), id, UpdateInstanceRequest{ExpiresAt: &expires})
	require.NoError(t, err)
	require.Equal(t, expires, *updated.ExpiresAt)

	ttl = 0
	updated, err = m.UpdateInstance(context.Background(), id, UpdateInstanceRequest{TTL: &ttl, RestartPolicySet: true})
	require.NoError(t, err)
	require.Nil(t, updated.ExpiresAt)
	require.Nil(t, updated.RestartPolicy)
	persisted, err := m.loadMetadata(id)
	require.NoError(t, err)
	require.Nil(t, persisted.ExpiresAt)
	require.Nil(t, persisted.RestartPolicy)
}

func TestMacOSUpdateRejectsUnsupportedFields(t *testing.T) {
	stored := StoredMetadata{MacOS: testMacImage().MacOS}
	for _, req := range []UpdateInstanceRequest{
		{Env: map[string]string{"TOKEN": "value"}},
		{AutoStandby: &autostandby.Policy{Enabled: true}},
		{HealthCheck: &healthcheck.Policy{Type: healthcheck.TypeTCP, TCP: &healthcheck.TCPCheck{Port: 80}}},
		{RestartPolicySet: true, RestartPolicy: &restartpolicy.Policy{Policy: restartpolicy.PolicyOnFailure}},
	} {
		require.ErrorIs(t, checkMacOSUpdate(&stored, req), ErrInvalidRequest)
		require.NoError(t, checkMacOSUpdate(&StoredMetadata{}, req), "Linux admission is unchanged")
	}
}

func TestMacOSCreateRestartPolicy(t *testing.T) {
	req := CreateInstanceRequest{RestartPolicy: &restartpolicy.Policy{Policy: restartpolicy.PolicyAlways}}
	require.NoError(t, prepareMacOSCreate(&req, testMacImage(), testMacOSCaps()))
	req = CreateInstanceRequest{RestartPolicy: &restartpolicy.Policy{Policy: restartpolicy.PolicyOnFailure}}
	require.ErrorIs(t, prepareMacOSCreate(&req, testMacImage(), testMacOSCaps()), ErrInvalidRequest)
}

func TestMacOSPlatformForCreateDoesNotMutateImage(t *testing.T) {
	platform := testMacImage().MacOS
	configured := macOSPlatformForCreate(platform, 4<<30, 3)
	require.Equal(t, uint64(4<<30), configured.Memory)
	require.Equal(t, uint(3), configured.CPUs)
	require.Equal(t, uint64(8<<30), platform.Memory)
	require.Equal(t, uint(4), platform.CPUs)
	require.Nil(t, macOSPlatformForCreate(nil, 1<<30, 2))
}
