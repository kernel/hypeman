//go:build linux

package devices

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"
)

// nvmlTimeout bounds the creatable-placements query, which takes about 20 ms
// on a two-GPU L40S host and stayed under 140 ms with every host core
// saturated.
var nvmlTimeout = 5 * time.Second

var (
	errNVMLTimeout   = errors.New("NVML query timed out")
	errNVMLStuck     = errors.New("an earlier NVML query is still running after its timeout")
	errNVMLEqualSize = errors.New("not in mixed-size vGPU mode")
)

// nvmlStuck counts timed-out NVML queries that have not returned.
var nvmlStuck atomic.Int32

// nvmlFallbacks counts creatable-placement queries answered from the sysfs
// lists for at least one GPU, by reason.
var nvmlFallbacks = map[string]*atomic.Int64{
	"error":      {},
	"equal_size": {},
	"timeout":    {},
	"stuck":      {},
}

// NVMLFallbacks returns how many creatable-placement queries have fallen back
// to the sysfs lists for at least one GPU since startup, by reason: NVML
// failed (error), every GPU it failed for is in equal-size mode (equal_size),
// it timed out (timeout), or an earlier timed-out query had not returned yet
// (stuck).
func NVMLFallbacks() map[string]int64 {
	counts := make(map[string]int64, len(nvmlFallbacks))
	for reason, count := range nvmlFallbacks {
		counts[reason] = count.Load()
	}
	return counts
}

func recordNVMLFallback(err error) {
	reason := "error"
	switch {
	case errors.Is(err, errNVMLStuck):
		reason = "stuck"
	case errors.Is(err, errNVMLTimeout):
		reason = "timeout"
	case onlyEqualSize(err):
		reason = "equal_size"
	}
	nvmlFallbacks[reason].Add(1)
	slog.Default().Warn("falling back to sysfs creatable vGPU types", "reason", reason, "error", err)
}

// onlyEqualSize reports whether every GPU NVML failed for was left out only
// because it is in equal-size mode, which happens when a driver reload or
// GPU reset drops mixed-size mode, rather than because NVML failed.
func onlyEqualSize(err error) bool {
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return errors.Is(err, errNVMLEqualSize)
	}
	for _, gpuErr := range joined.Unwrap() {
		if !errors.Is(gpuErr, errNVMLEqualSize) {
			return false
		}
	}
	return true
}

// creatableProfile is a profile a GPU can still create, with the placement
// IDs a new vGPU of that profile could start at.
type creatableProfile struct {
	VGPUProfileType
	placements []int
}

// creatableProfiles asks the driver, through NVML, which profiles the GPU of
// each VF can still create and at which placements, keyed by parent GPU
// address. Unlike the per-VF creatable_vgpu_types lists, this stays accurate
// on a busy GPU in mixed-size mode; see GPU.md. GPUs NVML could not answer
// for are missing from the result, and the error says why.
func (s vendorVFIOSysfs) creatableProfiles(vfs []VirtualFunction) (map[string][]creatableProfile, error) {
	query := s.creatableFunc
	if query == nil {
		query = nvmlCreatableProfiles
	}
	seen := make(map[string]struct{})
	gpus := make([]string, 0)
	for _, vf := range vfs {
		if _, ok := seen[vf.ParentGPU]; !ok {
			seen[vf.ParentGPU] = struct{}{}
			gpus = append(gpus, vf.ParentGPU)
		}
	}
	sort.Strings(gpus)
	byGPU, err := runBounded(func() (map[string][]creatableProfile, error) { return query(gpus) })
	if err != nil {
		return byGPU, fmt.Errorf("query creatable vGPU placements: %w", err)
	}
	return byGPU, nil
}

// creatableOrSysfs returns the NVML creatable profiles by GPU and the VFs
// whose GPU NVML could not answer for, which fall back to the sysfs lists.
func (s vendorVFIOSysfs) creatableOrSysfs(vfs []VirtualFunction) (map[string][]creatableProfile, []VirtualFunction) {
	creatable, err := s.creatableProfiles(vfs)
	if err != nil {
		recordNVMLFallback(err)
	}
	sysfsVFs := make([]VirtualFunction, 0)
	for _, vf := range vfs {
		if _, ok := creatable[vf.ParentGPU]; !ok {
			sysfsVFs = append(sysfsVFs, vf)
		}
	}
	return creatable, sysfsVFs
}

// runBounded returns at nvmlTimeout even if query has not: a call stuck in
// the driver cannot be interrupted. Until such a query returns, later calls
// fail at once instead of each waiting out the timeout, which the claim
// would pay while holding the vGPU allocation lock.
func runBounded[T any](query func() (T, error)) (T, error) {
	var zero T
	if nvmlStuck.Load() > 0 {
		return zero, errNVMLStuck
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := query()
		done <- result{value, err}
	}()
	timer := time.NewTimer(nvmlTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.value, r.err
	case <-timer.C:
	}
	nvmlStuck.Add(1)
	go func() {
		<-done
		nvmlStuck.Add(-1)
	}()
	return zero, fmt.Errorf("%w after %s", errNVMLTimeout, nvmlTimeout)
}
