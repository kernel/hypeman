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
	}{
		{"alone", 0, 30 * time.Second, 5 * time.Second},
		{"26 teardowns, TTL reaper", 25, 30 * time.Second, 20 * time.Second},
		{"61 teardowns, API", 60, time.Minute, 50 * time.Second},
		{"queue allowance cap", 80, 2 * time.Minute, 65 * time.Second},
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
				exited, err := teardown.Wait(ctx, VFIOTermGrace, 5*time.Second, func(d time.Duration) bool {
					time.Sleep(d)
					return false
				})
				require.NoError(t, err)
				require.False(t, exited)
				require.Equal(t, tc.wantWait, time.Since(start))
			})
		})
	}
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
