package devices

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// VFHealthRecord tracks a virtual function quarantined after a wedge
// conviction. A quarantined VF is excluded from vGPU placement until the
// parent GPU is SR-IOV cycled and the record cleared.
type VFHealthRecord struct {
	VFAddress     string    `json:"vf_address"`
	InstanceID    string    `json:"instance_id,omitempty"`
	SentinelLine  string    `json:"sentinel_line,omitempty"`
	QuarantinedAt time.Time `json:"quarantined_at"`
}

// VFQuarantine describes a wedge conviction to record.
type VFQuarantine struct {
	VFAddress    string
	InstanceID   string
	SentinelLine string
}

type vfHealthStore struct {
	mu      sync.Mutex
	path    string
	records map[string]VFHealthRecord
	// loadErr remembers a failed load of an existing state file. While set,
	// mutations are refused: persisting the empty in-memory store would
	// permanently clobber every previously persisted quarantine. Mutating
	// calls retry the load first so a transient read error self-heals.
	loadErr error
}

var vfHealth = &vfHealthStore{records: make(map[string]VFHealthRecord)}

// initVFHealthStore points the store at its state file and loads any
// persisted quarantines. Called from NewManager during startup wiring.
func initVFHealthStore(path string) error {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	vfHealth.path = path
	return vfHealth.loadLocked()
}

func (s *vfHealthStore) loadLocked() error {
	s.records = make(map[string]VFHealthRecord)
	s.loadErr = nil

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.loadErr = fmt.Errorf("read VF health state: %w", err)
		return s.loadErr
	}
	var records []VFHealthRecord
	if err := json.Unmarshal(data, &records); err != nil {
		s.loadErr = fmt.Errorf("unmarshal VF health state: %w", err)
		return s.loadErr
	}
	for _, record := range records {
		s.records[record.VFAddress] = record
	}
	return nil
}

// ensureLoadedLocked retries a previously failed load. Mutations must not
// proceed on a store that failed to load its existing state file.
func (s *vfHealthStore) ensureLoadedLocked() error {
	if s.loadErr == nil {
		return nil
	}
	return s.loadLocked()
}

// checkedAddresses returns the quarantined VF addresses, retrying a failed
// load first. Placement must not run against an empty record set that only
// exists because the state file was unreadable: that would put every
// previously quarantined VF back into rotation.
func (s *vfHealthStore) checkedAddresses() (map[string]struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(); err != nil {
		return nil, fmt.Errorf("VF health state unavailable: %w", err)
	}
	addresses := make(map[string]struct{}, len(s.records))
	for address := range s.records {
		addresses[address] = struct{}{}
	}
	return addresses, nil
}

// QuarantineVF records a wedge conviction for a VF and persists it. It takes
// the vGPU placement lock so a convicted VF is never concurrently selected
// for a new assignment. A conviction of an already-quarantined VF leaves the
// existing record unchanged and reports existed=true — one wedge produces
// one record no matter how many victim boots report it.
func QuarantineVF(q VFQuarantine) (existed bool, err error) {
	withVGPUPlacementLock(func() {
		existed, err = vfHealth.quarantine(q)
	})
	return existed, err
}

// VFHealthStoreUnavailable reports whether the persisted VF health state
// failed to load. While true, quarantine mutations are refused and vGPU
// placement fails closed — and the in-memory record set is empty, so the
// quarantine gauge would otherwise read zero exactly when quarantines exist
// but are unreadable. Surface this state instead of hiding it.
func VFHealthStoreUnavailable() bool {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	return vfHealth.loadErr != nil
}

// QuarantinedVFs returns all quarantine records, ordered by VF address.
func QuarantinedVFs() []VFHealthRecord {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	return vfHealth.sortedRecordsLocked()
}

// sortedRecordsLocked returns every quarantine record ordered by VF address.
// The caller must hold s.mu.
func (s *vfHealthStore) sortedRecordsLocked() []VFHealthRecord {
	records := make([]VFHealthRecord, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].VFAddress < records[j].VFAddress })
	return records
}

func (s *vfHealthStore) quarantine(q VFQuarantine) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(); err != nil {
		return false, err
	}
	if _, ok := s.records[q.VFAddress]; ok {
		return true, nil
	}
	s.records[q.VFAddress] = VFHealthRecord{
		VFAddress:     q.VFAddress,
		InstanceID:    q.InstanceID,
		SentinelLine:  q.SentinelLine,
		QuarantinedAt: time.Now().UTC(),
	}
	if err := s.persistLocked(); err != nil {
		// A quarantine is only real once it is on disk. Keeping the record in
		// memory would make the next report look like a repeat conviction and
		// end retries, leaving nothing persisted for the next restart.
		delete(s.records, q.VFAddress)
		return false, err
	}
	return false, nil
}

func (s *vfHealthStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.sortedRecordsLocked(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal VF health state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("create VF health state dir: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create VF health state: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write VF health state: %w", err)
	}
	// A quarantine is only real once it is on disk: sync before rename so a
	// host crash cannot leave an empty or partial file where a durable
	// conviction should be.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync VF health state: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close VF health state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename VF health state: %w", err)
	}
	// The rename already committed the quarantine, so a directory-sync
	// failure must not roll back the in-memory record. Report the weaker
	// crash-durability guarantee instead.
	dirPath := filepath.Dir(s.path)
	dir, err := os.Open(dirPath)
	if err != nil {
		slog.Default().Warn("failed to open VF health state directory for sync", "path", dirPath, "error", err)
		return nil
	}
	if err := dir.Sync(); err != nil {
		slog.Default().Warn("failed to sync VF health state directory", "path", dirPath, "error", err)
	}
	_ = dir.Close()
	return nil
}
