package devices

import (
	"encoding/json"
	"fmt"
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
	WedgeCount    int       `json:"wedge_count"`
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
}

var vfHealth = &vfHealthStore{records: make(map[string]VFHealthRecord)}

// initVFHealthStore points the store at its state file and loads any
// persisted quarantines. Called from NewManager during startup wiring.
func initVFHealthStore(path string) error {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	vfHealth.path = path
	vfHealth.records = make(map[string]VFHealthRecord)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read VF health state: %w", err)
	}
	var records []VFHealthRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("unmarshal VF health state: %w", err)
	}
	for _, record := range records {
		vfHealth.records[record.VFAddress] = record
	}
	return nil
}

// QuarantineVF records a wedge conviction for a VF and persists it. It takes
// the vGPU placement lock so a convicted VF is never concurrently selected
// for a new assignment. Repeat convictions on the same VF increment its
// wedge count and keep the original quarantine time.
func QuarantineVF(q VFQuarantine) (VFHealthRecord, error) {
	var record VFHealthRecord
	var err error
	withVGPUPlacementLock(func() {
		record, err = vfHealth.quarantine(q)
	})
	return record, err
}

// ClearVFQuarantine removes a VF's quarantine record, returning whether one
// existed. Callers clear a VF only after the parent GPU has been SR-IOV
// cycled and a verification boot came back clean.
func ClearVFQuarantine(vfAddress string) (bool, error) {
	var cleared bool
	var err error
	withVGPUPlacementLock(func() {
		cleared, err = vfHealth.clear(vfAddress)
	})
	return cleared, err
}

// QuarantinedVFs returns all quarantine records, ordered by VF address.
func QuarantinedVFs() []VFHealthRecord {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	records := make([]VFHealthRecord, 0, len(vfHealth.records))
	for _, record := range vfHealth.records {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].VFAddress < records[j].VFAddress })
	return records
}

func (s *vfHealthStore) quarantine(q VFQuarantine) (VFHealthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[q.VFAddress]
	if !ok {
		record = VFHealthRecord{
			VFAddress:     q.VFAddress,
			QuarantinedAt: time.Now().UTC(),
		}
	}
	record.WedgeCount++
	record.InstanceID = q.InstanceID
	record.SentinelLine = q.SentinelLine
	s.records[q.VFAddress] = record
	if err := s.persistLocked(); err != nil {
		return record, err
	}
	return record, nil
}

func (s *vfHealthStore) clear(vfAddress string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[vfAddress]; !ok {
		return false, nil
	}
	delete(s.records, vfAddress)
	return true, s.persistLocked()
}

// snapshotAddresses returns the set of quarantined VF addresses.
func (s *vfHealthStore) snapshotAddresses() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	addresses := make(map[string]struct{}, len(s.records))
	for address := range s.records {
		addresses[address] = struct{}{}
	}
	return addresses
}

func (s *vfHealthStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	records := make([]VFHealthRecord, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].VFAddress < records[j].VFAddress })
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal VF health state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("create VF health state dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write VF health state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename VF health state: %w", err)
	}
	return nil
}
