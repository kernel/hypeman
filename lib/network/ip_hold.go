package network

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

// ipReuseHold keeps a guest IP unassigned after it leaves the allocation set, for
// longer than clients keep dialing an address they resolved before the guest was
// deleted, so a late connection cannot reach the next guest given that IP.
const ipReuseHold = time.Minute

const ipReleasesFile = "guest-ip-releases.json"

// HoldReleasedIP keeps ip out of new allocations for ipReuseHold. Callers record the
// hold before the IP leaves the allocation set. The hold is persisted so a restart
// does not end it early; if that write fails the hold still applies until restart.
func (m *manager) HoldReleasedIP(ctx context.Context, ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heldIPsLocked(ctx)
	m.ipReleases[ip] = time.Now()
	if err := m.saveIPReleasesLocked(); err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to persist released guest IP; hold will not survive a restart", "ip", ip, "error", err)
	}
}

func (m *manager) saveIPReleasesLocked() error {
	data, err := json.Marshal(m.ipReleases)
	if err != nil {
		return err
	}
	path := filepath.Join(m.paths.DataDir(), ipReleasesFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// heldIPsLocked returns the IPs still in their reuse hold, dropping expired ones.
// Holds are loaded from disk on first use. The caller holds m.mu.
func (m *manager) heldIPsLocked(ctx context.Context) map[string]bool {
	if m.ipReleases == nil {
		m.ipReleases = m.loadIPReleases(ctx)
	}
	now := time.Now()
	held := make(map[string]bool, len(m.ipReleases))
	for ip, at := range m.ipReleases {
		if now.Sub(at) >= ipReuseHold {
			delete(m.ipReleases, ip)
			continue
		}
		held[ip] = true
	}
	return held
}

func (m *manager) loadIPReleases(ctx context.Context) map[string]time.Time {
	releases := make(map[string]time.Time)
	data, err := os.ReadFile(filepath.Join(m.paths.DataDir(), ipReleasesFile))
	if errors.Is(err, os.ErrNotExist) {
		return releases
	}
	if err == nil {
		err = json.Unmarshal(data, &releases)
	}
	if err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "ignoring unreadable released guest IPs", "error", err)
		return make(map[string]time.Time)
	}
	return releases
}
