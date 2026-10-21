package instances

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
)

const (
	hypervisorStateCacheTTL = 5 * time.Second
	getVMInfoTimeout        = 500 * time.Millisecond
)

type hypervisorStateCacheEntry struct {
	state       hypervisor.VMState
	refreshedAt time.Time
}

type instanceStateQueryLock struct {
	sync.RWMutex
	cacheMu    sync.Mutex
	generation uint64
}

func (m *manager) queryHypervisorState(ctx context.Context, stored *StoredMetadata) (hypervisorStateQueryResult, error) {
	if err := ctx.Err(); err != nil {
		return hypervisorStateQueryResult{}, err
	}
	lock := m.hypervisorStateQueryLock(stored.Id)
	if !lock.TryRLock() {
		return hypervisorStateQueryResult{operationInProgress: true}, nil
	}
	defer lock.RUnlock()

	waitCtx, cancelWait := context.WithTimeout(ctx, getVMInfoTimeout)
	defer cancelWait()
	result := m.hypervisorStateQueries.DoChan(stored.Id, func() (any, error) {
		return m.runHypervisorStateQuery(context.WithoutCancel(ctx), stored, lock)
	})
	select {
	case <-waitCtx.Done():
		return hypervisorStateQueryResult{}, waitCtx.Err()
	case result := <-result:
		if result.Err != nil {
			return hypervisorStateQueryResult{}, result.Err
		}
		return result.Val.(hypervisorStateQueryResult), nil
	}
}

func (m *manager) runHypervisorStateQuery(ctx context.Context, stored *StoredMetadata, lock *instanceStateQueryLock) (hypervisorStateQueryResult, error) {
	if !lock.TryRLock() {
		return hypervisorStateQueryResult{operationInProgress: true}, nil
	}
	defer lock.RUnlock()

	if cached, ok := m.loadCachedHypervisorState(stored.Id); ok {
		return hypervisorStateQueryResult{state: cached, cacheHit: true}, nil
	}

	lock.cacheMu.Lock()
	generation := lock.generation
	lock.cacheMu.Unlock()

	log := logger.FromContext(ctx)
	queryCtx, cancelQuery := context.WithTimeout(ctx, getVMInfoTimeout)
	defer cancelQuery()

	hv, err := m.getHypervisor(stored.SocketPath, stored.HypervisorType)
	if err != nil {
		log.WarnContext(ctx, "failed to determine instance state",
			"instance_id", stored.Id,
			"socket", stored.SocketPath,
			"error", err,
		)
		return hypervisorStateQueryResult{}, fmt.Errorf("failed to create hypervisor client: %w", err)
	}

	info, err := hv.GetVMInfo(queryCtx)
	if err != nil {
		log.WarnContext(ctx, "failed to query hypervisor state",
			"instance_id", stored.Id,
			"socket", stored.SocketPath,
			"error", err,
		)
		return hypervisorStateQueryResult{}, fmt.Errorf("failed to query hypervisor: %w", err)
	}
	if !m.storeQueriedHypervisorState(lock, stored.Id, info.State, generation) {
		return hypervisorStateQueryResult{}, fmt.Errorf("hypervisor state changed during query")
	}
	return hypervisorStateQueryResult{state: info.State}, nil
}

type hypervisorStateQueryResult struct {
	state               hypervisor.VMState
	cacheHit            bool
	operationInProgress bool
}

// hypervisorStateQueryLock is shared by state probes and standby snapshots.
func (m *manager) hypervisorStateQueryLock(id string) *instanceStateQueryLock {
	lock, _ := m.hypervisorStateQueryLocks.LoadOrStore(id, &instanceStateQueryLock{})
	return lock.(*instanceStateQueryLock)
}

func (m *manager) tryLockHypervisorStateForStandby(id string) *instanceStateQueryLock {
	lock := m.hypervisorStateQueryLock(id)
	if !lock.TryLock() {
		return nil
	}
	return lock
}

// loadCachedHypervisorState returns a non-expired cached state.
func (m *manager) loadCachedHypervisorState(id string) (hypervisor.VMState, bool) {
	v, ok := m.hypervisorStateCache.Load(id)
	if !ok {
		return "", false
	}
	entry, ok := v.(hypervisorStateCacheEntry)
	if !ok {
		return "", false
	}
	if m.nowUTC().Sub(entry.refreshedAt) > hypervisorStateCacheTTL {
		return "", false
	}
	return entry.state, true
}

// storeCachedHypervisorState records a lifecycle-observed hypervisor state.
func (m *manager) storeCachedHypervisorState(id string, state hypervisor.VMState) {
	lock := m.hypervisorStateQueryLock(id)
	lock.cacheMu.Lock()
	lock.generation++
	m.hypervisorStateCache.Store(id, hypervisorStateCacheEntry{
		state:       state,
		refreshedAt: m.nowUTC(),
	})
	lock.cacheMu.Unlock()
}

func (m *manager) storeQueriedHypervisorState(lock *instanceStateQueryLock, id string, state hypervisor.VMState, generation uint64) bool {
	lock.cacheMu.Lock()
	defer lock.cacheMu.Unlock()
	if lock.generation != generation {
		return false
	}
	lock.generation++
	m.hypervisorStateCache.Store(id, hypervisorStateCacheEntry{
		state:       state,
		refreshedAt: m.nowUTC(),
	})
	return true
}

// invalidateCachedHypervisorState also fences any in-flight state query.
func (m *manager) invalidateCachedHypervisorState(id string) {
	lock := m.hypervisorStateQueryLock(id)
	lock.cacheMu.Lock()
	lock.generation++
	m.hypervisorStateCache.Delete(id)
	lock.cacheMu.Unlock()
}

// updateCachedHypervisorStateFromInstance mirrors lifecycle state into the cache.
func (m *manager) updateCachedHypervisorStateFromInstance(inst *Instance) {
	if inst == nil {
		return
	}
	switch inst.State {
	case StateRunning, StateInitializing:
		m.storeCachedHypervisorState(inst.Id, hypervisor.StateRunning)
	case StateCreated:
		m.storeCachedHypervisorState(inst.Id, hypervisor.StateCreated)
	case StatePaused:
		m.storeCachedHypervisorState(inst.Id, hypervisor.StatePaused)
	case StateShutdown:
		m.storeCachedHypervisorState(inst.Id, hypervisor.StateShutdown)
	default:
		// Stopped, Standby, Unknown — no live socket worth caching.
		m.invalidateCachedHypervisorState(inst.Id)
	}
}
