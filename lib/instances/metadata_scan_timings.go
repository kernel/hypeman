package instances

import (
	"time"

	"go.opentelemetry.io/otel/attribute"
)

type metadataScanStep int

const (
	metadataScanStepListDir metadataScanStep = iota
	metadataScanStepStat
	metadataScanStepAliasLockWait
	metadataScanStepRead
	metadataScanStepDecode
	metadataScanStepHydrate
	metadataScanStepCount
)

var metadataScanStepNames = [metadataScanStepCount]string{
	metadataScanStepListDir:       "list_dir",
	metadataScanStepStat:          "stat",
	metadataScanStepAliasLockWait: "alias_lock_wait",
	metadataScanStepRead:          "read",
	metadataScanStepDecode:        "decode",
	metadataScanStepHydrate:       "hydrate",
}

// metadataScanTimings splits a scan over every instance's metadata into its
// steps, so a slow scan can be attributed to directory enumeration, the
// snapshot source alias lock, file reads, or decoding, and to the instance
// whose step was slowest.
type metadataScanTimings struct {
	total [metadataScanStepCount]time.Duration
	seen  [metadataScanStepCount]bool

	slowest     time.Duration
	slowestStep metadataScanStep
	slowestID   string
}

// lap adds the time since start to step and returns the current time, which
// starts the next step. A nil receiver records nothing.
func (t *metadataScanTimings) lap(step metadataScanStep, instanceID string, start time.Time) time.Time {
	now := time.Now()
	if t == nil {
		return now
	}
	elapsed := now.Sub(start)
	t.total[step] += elapsed
	t.seen[step] = true
	if elapsed > t.slowest {
		t.slowest = elapsed
		t.slowestStep = step
		t.slowestID = instanceID
	}
	return now
}

func (t *metadataScanTimings) attributes() []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, int(metadataScanStepCount)+3)
	for step, total := range t.total {
		if !t.seen[step] {
			continue
		}
		attrs = append(attrs, attribute.Int64(metadataScanStepNames[step]+"_ms", total.Milliseconds()))
	}
	if t.slowest > 0 {
		attrs = append(attrs,
			attribute.String("slowest_step", metadataScanStepNames[t.slowestStep]),
			attribute.Int64("slowest_step_ms", t.slowest.Milliseconds()),
			attribute.String("slowest_step_instance_id", t.slowestID),
		)
	}
	return attrs
}
