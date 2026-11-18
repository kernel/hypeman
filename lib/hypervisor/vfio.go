package hypervisor

import (
	"context"
	"sync/atomic"
	"time"
)

// VFIOTermGrace allows VFIO teardown to finish after SIGTERM before SIGKILL;
// SIGKILL during initialization can wedge the VF.
const VFIOTermGrace = 5 * time.Second

var activeVFIOTeardowns atomic.Int32

// VFIOTeardown counts a VMM against the host driver until it exits or its
// caller gives up, including time spent waiting after SIGKILL.
type VFIOTeardown struct {
	atStart int32
}

func BeginVFIOTeardown() VFIOTeardown {
	return VFIOTeardown{atStart: activeVFIOTeardowns.Add(1)}
}

func (VFIOTeardown) Done() { activeVFIOTeardowns.Add(-1) }

// Wait allows one extra second per overlapping teardown, up to a minute:
// 16 simultaneous vGPU deletes can take 10s in the driver after SIGTERM.
// It leaves reserve before the caller's deadline for escalation and cleanup.
// False with no error means the caller may escalate; cancellation or a budget
// too short for the base grace is not a reason to hard-kill.
func (t VFIOTeardown) Wait(ctx context.Context, base, reserve time.Duration, waitForExit func(time.Duration) bool) (bool, error) {
	start := time.Now()
	limit := base + time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, deadline.Sub(start)-reserve)
	}
	peak := t.atStart
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		peak = max(peak, activeVFIOTeardowns.Load())
		grace := min(base+time.Duration(peak-1)*time.Second, limit)
		remaining := grace - time.Since(start)
		if remaining <= 0 {
			if limit < base {
				return false, context.DeadlineExceeded
			}
			return false, nil
		}
		if waitForExit(min(remaining, 200*time.Millisecond)) {
			return true, nil
		}
	}
}
