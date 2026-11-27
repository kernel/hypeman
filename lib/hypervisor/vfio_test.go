package hypervisor

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVFIOTeardownWaitBudget(t *testing.T) {
	for _, tc := range []struct {
		name             string
		others           int
		budget, wantWait time.Duration
		wantErr          error
	}{
		{"alone", 0, 30 * time.Second, 5 * time.Second, nil},
		{"just enough for escalation", 0, 15 * time.Second, 5 * time.Second, nil},
		{"base fits but escalation does not", 0, 13 * time.Second, 5 * time.Second, context.DeadlineExceeded},
		{"26 teardowns, TTL reaper", 25, 30 * time.Second, 20 * time.Second, nil},
		{"61 teardowns, API", 60, time.Minute, 50 * time.Second, nil},
		{"queue allowance cap", 80, 2 * time.Minute, 65 * time.Second, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), tc.budget)
				defer cancel()
				time.Sleep(5 * time.Second) // guest shutdown before SIGTERM
				for range tc.others {
					defer BeginVFIOTeardown().Done()
				}
				teardown := BeginVFIOTeardown()
				defer teardown.Done()
				start := time.Now()
				exited, err := teardown.Wait(ctx, VFIOTermGrace, 4*time.Second, func(d time.Duration) bool {
					time.Sleep(d)
					return false
				})
				require.ErrorIs(t, err, tc.wantErr)
				require.False(t, exited)
				require.Equal(t, tc.wantWait, time.Since(start))
			})
		})
	}
}

func TestVFIOTeardownShortBudgetAllowsCleanExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		teardown := BeginVFIOTeardown()
		defer teardown.Done()
		start := time.Now()
		exited, err := teardown.Wait(ctx, VFIOTermGrace, 4*time.Second, func(d time.Duration) bool {
			time.Sleep(d)
			return time.Since(start) >= 4*time.Second
		})
		require.NoError(t, err, "waited %s for an exit at 4s", time.Since(start))
		require.True(t, exited)
		require.Equal(t, 4*time.Second, time.Since(start))
	})
}

func TestVFIOTeardownKeepsLatePeak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := BeginVFIOTeardown()
		defer first.Done()
		go func() {
			time.Sleep(time.Second)
			peer := BeginVFIOTeardown()
			defer peer.Done()
			time.Sleep(time.Second)
		}()
		start := time.Now()
		exited, err := first.Wait(t.Context(), VFIOTermGrace, 0, func(d time.Duration) bool {
			time.Sleep(d)
			return false
		})
		require.NoError(t, err)
		require.False(t, exited)
		require.Equal(t, VFIOTermGrace+time.Second, time.Since(start))
	})
}
