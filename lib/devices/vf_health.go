package devices

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

const (
	vfHealthFileVersion          = 1
	defaultVFQuarantineThreshold = 2
)

type vfInitFailure struct {
	InstanceID string    `json:"instance_id,omitempty"`
	AssignedAt string    `json:"assigned_at,omitempty"`
	ReportedAt time.Time `json:"reported_at"`
}

type vfHealthRecord struct {
	VFAddress     string          `json:"vf_address"`
	Failures      []vfInitFailure `json:"failures,omitempty"`
	QuarantinedAt *time.Time      `json:"quarantined_at,omitempty"`
}

type vfHealthFile struct {
	Version int              `json:"version"`
	Records []vfHealthRecord `json:"records"`
}

// VFInitFailureReport describes one guest-reported driver init failure.
type VFInitFailureReport struct {
	VFAddress  string
	InstanceID string
	AssignedAt string
}

// VFInitSuccessReport identifies the assignment that successfully initialized.
type VFInitSuccessReport struct {
	VFAddress  string
	InstanceID string
	AssignedAt string
}

// VFReportOutcome describes how a failure report changed a VF's health state.
type VFReportOutcome int

const (
	// VFReportUnchanged means the VF was already quarantined or this
	// assignment was already recorded.
	VFReportUnchanged VFReportOutcome = iota
	// VFReportRecorded means the failure was tallied below the quarantine threshold.
	VFReportRecorded
	// VFReportQuarantined means this report crossed the threshold and quarantined the VF.
	VFReportQuarantined
)

// VFReportResult is the outcome of recording a driver init failure.
type VFReportResult struct {
	Outcome   VFReportOutcome
	Failures  int
	Threshold int
}

// VFSuccessResult describes how a successful init changed a VF's health state.
type VFSuccessResult struct {
	Cleared   int
	Rescinded bool
}

type vfHealthStore struct {
	mu          sync.Mutex
	path        string
	records     map[string]vfHealthRecord
	threshold   int
	loadErr     error
	persistErr  error
	syncDirFunc func(string) error
}

var vfHealthAddressPattern = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

var (
	vfHealth = &vfHealthStore{
		records:     make(map[string]vfHealthRecord),
		threshold:   defaultVFQuarantineThreshold,
		syncDirFunc: syncDir,
	}
	// vendorVFIOMu is acquired before vfHealth.mu. It serializes quarantine
	// mutations with vendor-VFIO create, destroy, and reconciliation so
	// placement cannot select a VF while it is being quarantined.
	vendorVFIOMu sync.Mutex
)

func initVFHealth(path string) error {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	vfHealth.path = path
	return vfHealth.loadLocked()
}

// SetVFQuarantineThreshold configures the number of failed assignments
// required to quarantine a VF. Already-recorded tallies are re-evaluated so a
// lowered threshold applies to failures persisted before the change.
func SetVFQuarantineThreshold(n int) {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	vfHealth.threshold = n
	vfHealth.requarantineLocked()
}

// requarantineLocked quarantines records whose failure tallies meet the
// current threshold, so threshold changes and loaded state agree.
func (s *vfHealthStore) requarantineLocked() {
	changed := false
	for address, record := range s.records {
		if record.QuarantinedAt != nil || len(record.Failures) < s.threshold {
			continue
		}
		now := time.Now().UTC()
		record.QuarantinedAt = &now
		s.records[address] = record
		changed = true
	}
	if changed {
		if _, err := s.persistLocked(); err != nil {
			slog.Default().Error("failed to persist re-evaluated VF quarantines; vGPU placement is disabled until a write succeeds", "error", err)
		}
	}
}

func (s *vfHealthStore) loadLocked() error {
	s.records = make(map[string]vfHealthRecord)
	s.loadErr = nil
	s.persistErr = nil

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.loadErr = fmt.Errorf("read VF health state: %w", err)
		return s.loadErr
	}
	var state vfHealthFile
	if err := json.Unmarshal(data, &state); err != nil {
		s.loadErr = fmt.Errorf("unmarshal VF health state: %w", err)
		return s.loadErr
	}
	if state.Version != vfHealthFileVersion {
		s.loadErr = fmt.Errorf("validate VF health state: unsupported version %d", state.Version)
		return s.loadErr
	}
	if state.Records == nil {
		s.loadErr = fmt.Errorf("validate VF health state: expected a records array")
		return s.loadErr
	}
	loaded := make(map[string]vfHealthRecord, len(state.Records))
	for i, record := range state.Records {
		if !vfHealthAddressPattern.MatchString(record.VFAddress) {
			s.loadErr = fmt.Errorf("validate VF health state record %d: invalid VF address %q", i, record.VFAddress)
			return s.loadErr
		}
		if record.QuarantinedAt != nil && record.QuarantinedAt.IsZero() {
			s.loadErr = fmt.Errorf("validate VF health state record %d: missing quarantine timestamp", i)
			return s.loadErr
		}
		if record.QuarantinedAt == nil && len(record.Failures) == 0 {
			s.loadErr = fmt.Errorf("validate VF health state record %d: neither quarantined nor any recorded failures", i)
			return s.loadErr
		}
		assignments := make(map[string]struct{}, len(record.Failures))
		for j, failure := range record.Failures {
			if failure.ReportedAt.IsZero() {
				s.loadErr = fmt.Errorf("validate VF health state record %d failure %d: missing report timestamp", i, j)
				return s.loadErr
			}
			key := failure.InstanceID + "\x00" + failure.AssignedAt
			if _, exists := assignments[key]; exists {
				s.loadErr = fmt.Errorf("validate VF health state record %d: duplicate failure for assignment %q", i, failure.InstanceID)
				return s.loadErr
			}
			assignments[key] = struct{}{}
		}
		if _, exists := loaded[record.VFAddress]; exists {
			s.loadErr = fmt.Errorf("validate VF health state record %d: duplicate VF address %q", i, record.VFAddress)
			return s.loadErr
		}
		loaded[record.VFAddress] = record
	}
	s.records = loaded
	s.requarantineLocked()
	return nil
}

func (s *vfHealthStore) ensureLoadedLocked() error {
	if s.loadErr == nil {
		return nil
	}
	return s.loadLocked()
}

func (s *vfHealthStore) checkedAddresses() (map[string]struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(); err != nil {
		return nil, fmt.Errorf("VF health state unavailable: %w", err)
	}
	if s.persistErr != nil {
		return nil, fmt.Errorf("VF health state unavailable: last write failed: %w", s.persistErr)
	}
	addresses := make(map[string]struct{}, len(s.records))
	for address, record := range s.records {
		if record.QuarantinedAt != nil {
			addresses[address] = struct{}{}
		}
	}
	return addresses, nil
}

// VGPUAvailability returns free allocatable and quarantined VF counts.
func VGPUAvailability(framework VGPUFramework, vfs []VirtualFunction) (allocatable, quarantined int, err error) {
	if framework != VGPUFrameworkVendorVFIO {
		return countFreeVFs(vfs, nil), 0, nil
	}
	addresses, err := vfHealth.checkedAddresses()
	if err != nil {
		return 0, 0, err
	}
	for _, vf := range vfs {
		if _, ok := addresses[vf.PCIAddress]; ok {
			quarantined++
		}
	}
	return countFreeVFs(vfs, addresses), quarantined, nil
}

func countFreeVFs(vfs []VirtualFunction, quarantined map[string]struct{}) int {
	available := 0
	for _, vf := range vfs {
		if vf.Allocated {
			continue
		}
		if _, ok := quarantined[vf.PCIAddress]; !ok {
			available++
		}
	}
	return available
}

// ReportVFInitFailure records a guest-reported driver init failure and
// quarantines the VF once failures from enough distinct assignments accumulate.
func ReportVFInitFailure(report VFInitFailureReport) (VFReportResult, error) {
	vendorVFIOMu.Lock()
	defer vendorVFIOMu.Unlock()
	return vfHealth.reportFailure(report)
}

// ReportVFInitSuccess clears failures through an exactly matched successful
// assignment. A quarantine is rescinded only when that assignment triggered it.
func ReportVFInitSuccess(report VFInitSuccessReport) (VFSuccessResult, error) {
	vendorVFIOMu.Lock()
	defer vendorVFIOMu.Unlock()
	return vfHealth.reportSuccess(report)
}

// VFHealthStoreUnavailable reports whether persisted state failed to load or
// the last write failed.
func VFHealthStoreUnavailable() bool {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	return vfHealth.loadErr != nil || vfHealth.persistErr != nil
}

// TotalQuarantinedVFs returns the number of quarantined VFs in persisted state.
func TotalQuarantinedVFs() int {
	vfHealth.mu.Lock()
	defer vfHealth.mu.Unlock()
	count := 0
	for _, record := range vfHealth.records {
		if record.QuarantinedAt != nil {
			count++
		}
	}
	return count
}

func (s *vfHealthStore) sortedRecordsLocked() []vfHealthRecord {
	records := make([]vfHealthRecord, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].VFAddress < records[j].VFAddress })
	return records
}

func (s *vfHealthStore) reportFailure(report VFInitFailureReport) (VFReportResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(); err != nil {
		return VFReportResult{}, err
	}
	if !vfHealthAddressPattern.MatchString(report.VFAddress) {
		return VFReportResult{}, fmt.Errorf("invalid VF address %q", report.VFAddress)
	}
	if err := s.retryPersistLocked(); err != nil {
		return VFReportResult{}, err
	}

	previous, existed := s.records[report.VFAddress]
	result := VFReportResult{Failures: len(previous.Failures), Threshold: s.threshold}
	if previous.QuarantinedAt != nil {
		return result, nil
	}
	for _, failure := range previous.Failures {
		if sameVFAssignment(failure, report.InstanceID, report.AssignedAt) {
			return result, nil
		}
	}

	record := vfHealthRecord{
		VFAddress: report.VFAddress,
		Failures: append(append([]vfInitFailure(nil), previous.Failures...), vfInitFailure{
			InstanceID: report.InstanceID,
			AssignedAt: report.AssignedAt,
			ReportedAt: time.Now().UTC(),
		}),
	}
	result.Failures = len(record.Failures)
	result.Outcome = VFReportRecorded
	if result.Failures >= s.threshold {
		now := time.Now().UTC()
		record.QuarantinedAt = &now
		result.Outcome = VFReportQuarantined
	}
	s.records[report.VFAddress] = record
	renamed, err := s.persistLocked()
	if err != nil {
		if !renamed {
			if existed {
				s.records[report.VFAddress] = previous
			} else {
				delete(s.records, report.VFAddress)
			}
		}
		return VFReportResult{}, err
	}
	return result, nil
}

func sameVFAssignment(failure vfInitFailure, instanceID, assignedAt string) bool {
	return failure.InstanceID == instanceID && failure.AssignedAt == assignedAt
}

func (s *vfHealthStore) reportSuccess(report VFInitSuccessReport) (VFSuccessResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(); err != nil {
		return VFSuccessResult{}, err
	}
	if !vfHealthAddressPattern.MatchString(report.VFAddress) {
		return VFSuccessResult{}, fmt.Errorf("invalid VF address %q", report.VFAddress)
	}
	if err := s.retryPersistLocked(); err != nil {
		return VFSuccessResult{}, err
	}
	previous, ok := s.records[report.VFAddress]
	if !ok || len(previous.Failures) == 0 {
		return VFSuccessResult{}, nil
	}

	match := -1
	for i, failure := range previous.Failures {
		if sameVFAssignment(failure, report.InstanceID, report.AssignedAt) {
			match = i
			break
		}
	}
	if match < 0 || (previous.QuarantinedAt != nil && match != len(previous.Failures)-1) {
		return VFSuccessResult{}, nil
	}

	remaining := append([]vfInitFailure(nil), previous.Failures[match+1:]...)
	result := VFSuccessResult{
		Cleared:   len(previous.Failures) - len(remaining),
		Rescinded: previous.QuarantinedAt != nil,
	}
	if len(remaining) == 0 {
		delete(s.records, report.VFAddress)
	} else {
		record := previous
		record.Failures = remaining
		s.records[report.VFAddress] = record
	}
	renamed, err := s.persistLocked()
	if err != nil {
		if !renamed {
			s.records[report.VFAddress] = previous
		}
		return VFSuccessResult{}, err
	}
	return result, nil
}

func (s *vfHealthStore) retryPersistLocked() error {
	if s.persistErr == nil {
		return nil
	}
	_, err := s.persistLocked()
	return err
}

// persistLocked writes the current records to disk. A failure is latched and
// fails placement closed until a later write succeeds. The returned boolean
// reports whether the rename made the new state visible.
func (s *vfHealthStore) persistLocked() (bool, error) {
	if s.path == "" {
		return false, nil
	}
	renamed, err := s.writeStateLocked()
	s.persistErr = err
	return renamed, err
}

func (s *vfHealthStore) writeStateLocked() (bool, error) {
	data, err := json.MarshalIndent(vfHealthFile{
		Version: vfHealthFileVersion,
		Records: s.sortedRecordsLocked(),
	}, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshal VF health state: %w", err)
	}
	dirPath := filepath.Dir(s.path)
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return false, fmt.Errorf("create VF health state dir: %w", err)
	}
	if err := s.syncDirFunc(filepath.Dir(dirPath)); err != nil {
		return false, fmt.Errorf("sync VF health state parent dir: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return false, fmt.Errorf("create VF health state: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return false, fmt.Errorf("write VF health state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return false, fmt.Errorf("sync VF health state: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("close VF health state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("rename VF health state: %w", err)
	}
	if err := s.syncDirFunc(dirPath); err != nil {
		return true, fmt.Errorf("sync VF health state dir: %w", err)
	}
	return true, nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
