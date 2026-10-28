package builds

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// MachineBuildLimits applies only to the native machine build backend. Bytes are
// logical sizes: sparse files and APFS clones may become fully allocated.
type MachineBuildLimits struct {
	MaxConcurrent  int
	MaxExportBytes int64
	MinFreeBytes   int64
}

func (l MachineBuildLimits) Validate() error {
	if l.MaxConcurrent < 1 || l.MaxExportBytes <= 1<<20 || l.MinFreeBytes < 1 || l.MaxExportBytes > (math.MaxInt64-l.MinFreeBytes-(256<<20))/4 {
		return fmt.Errorf("machine build limits require positive concurrency, export size and headroom without overflow")
	}
	return nil
}

type machineReservation struct{ quarantined bool }

// MachineBuildStorage serializes admission against a single data filesystem.
// Reservations intentionally double-count already-written active job bytes.
// They are admission budgets, not filesystem quotas: unrelated writers are not
// synchronized and can still consume space after a check.
type MachineBuildStorage struct {
	mu        sync.Mutex
	limits    MachineBuildLimits
	root      string
	markers   string
	jobs      map[string]machineReservation
	available func(string) (int64, error)
}

func NewMachineBuildStorage(root string, limits MachineBuildLimits) (*MachineBuildStorage, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("machine storage root must be absolute")
	}
	if _, err := machineAvailableBytes(root); err != nil {
		return nil, err
	}
	s := &MachineBuildStorage{limits: limits, root: root, markers: filepath.Join(root, ".machine-build-reservations"), jobs: make(map[string]machineReservation), available: machineAvailableBytes}
	if err := os.MkdirAll(s.markers, 0700); err != nil {
		return nil, err
	}
	if err := syncMachineReservationDir(root); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.markers)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !validMachineReservationID(entry.Name()) || !entry.Type().IsRegular() {
			return nil, fmt.Errorf("invalid machine storage reservation")
		}
		data, err := os.ReadFile(filepath.Join(s.markers, entry.Name()))
		if err != nil {
			return nil, err
		}
		if string(data) != strconv.FormatInt(limits.MaxExportBytes, 10) {
			return nil, fmt.Errorf("machine storage reservation requires its original export limit")
		}
		s.jobs[entry.Name()] = machineReservation{quarantined: true}
	}
	return s, nil
}

func machineAvailableBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 || uint64(stat.Bavail) > uint64(math.MaxInt64)/uint64(stat.Bsize) {
		return 0, fmt.Errorf("invalid filesystem capacity")
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func (s *MachineBuildStorage) sameFilesystem(path string) error {
	var root, target unix.Statfs_t
	if err := unix.Statfs(s.root, &root); err != nil {
		return err
	}
	if err := unix.Statfs(path, &target); err != nil {
		return err
	}
	if root.Fsid != target.Fsid {
		return fmt.Errorf("machine build storage must use the admitted data filesystem")
	}
	return nil
}

func (s *MachineBuildStorage) reserve(ctx context.Context, id, workDir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validMachineReservationID(id) {
		return fmt.Errorf("invalid machine storage reservation ID")
	}
	if _, exists := s.jobs[id]; exists {
		return fmt.Errorf("machine build already holds storage")
	}
	if len(s.jobs) >= s.limits.MaxConcurrent {
		return fmt.Errorf("machine build concurrency limit reached")
	}
	if err := s.sameFilesystem(workDir); err != nil {
		return err
	}
	free, err := s.available(s.root)
	if err != nil {
		return err
	}
	// Budget four logical copies for working/export disks and publication's
	// blob, unpack and installed image stages (the working VM is deleted before
	// publication), plus source and metadata overhead. This is not a quota.
	perJob := 4*s.limits.MaxExportBytes + (256 << 20)
	if free < s.limits.MinFreeBytes || int64(len(s.jobs)+1) > (free-s.limits.MinFreeBytes)/perJob {
		return fmt.Errorf("insufficient machine build disk headroom")
	}
	// Persist before creating a VM. Restart must not forget a still-running
	// sparse working disk's potential growth after interrupted cleanup.
	marker, err := os.OpenFile(filepath.Join(s.markers, id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	s.jobs[id] = machineReservation{quarantined: true}
	_, writeErr := marker.WriteString(strconv.FormatInt(s.limits.MaxExportBytes, 10))
	syncErr := marker.Sync()
	closeErr := marker.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := syncMachineReservationDir(s.markers); err != nil {
		return err
	}
	s.jobs[id] = machineReservation{}
	return nil
}

// Source staging is serialized with admission so queued uploads cannot consume
// headroom promised to running jobs. No full-disk reservation is held while queued.
func (s *MachineBuildStorage) stageSource(path string, size int64, stage func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sameFilesystem(filepath.Dir(path)); err != nil {
		return err
	}
	free, err := s.available(s.root)
	if err != nil {
		return err
	}
	perJob := 4*s.limits.MaxExportBytes + (256 << 20)
	if size < 0 || free < s.limits.MinFreeBytes || size > free-s.limits.MinFreeBytes || int64(len(s.jobs)) > (free-s.limits.MinFreeBytes-size)/perJob {
		return fmt.Errorf("insufficient machine source disk headroom")
	}
	return stage()
}

func (s *MachineBuildStorage) finish(id string, clean bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[id] = machineReservation{quarantined: true}
	if clean {
		if err := os.Remove(filepath.Join(s.markers, id)); err != nil {
			return err
		}
		if err := syncMachineReservationDir(s.markers); err != nil {
			return err
		}
		delete(s.jobs, id)
	}
	return nil
}

func (s *MachineBuildStorage) quarantinedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0)
	for id, job := range s.jobs {
		if job.quarantined {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *MachineBuildStorage) quarantined(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id].quarantined
}

// A recovered job may release a quarantine only after its prior instance was reaped
// and any previously submitted image import is terminal.
func (s *MachineBuildStorage) reaped(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs[id].quarantined {
		if err := os.Remove(filepath.Join(s.markers, id)); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := syncMachineReservationDir(s.markers); err != nil {
			return err
		}
		delete(s.jobs, id)
	}
	return nil
}

func validMachineReservationID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789-") == ""
}

func syncMachineReservationDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (s *MachineBuildStorage) checkHeadroom() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	free, err := s.available(s.root)
	if err != nil {
		return err
	}
	perJob := 4*s.limits.MaxExportBytes + (256 << 20)
	if free < s.limits.MinFreeBytes || int64(len(s.jobs)) > (free-s.limits.MinFreeBytes)/perJob {
		return fmt.Errorf("insufficient machine build disk headroom")
	}
	return nil
}

func machineStorageSize(files []string, limit int64) error {
	for _, path := range files {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
			return fmt.Errorf("machine storage exceeds export size limit or is not a nonempty regular file")
		}
		limit -= info.Size()
	}
	return nil
}

func (s *MachineBuildStorage) checkBundle(root string) error {
	if err := s.sameFilesystem(root); err != nil {
		return err
	}
	return machineStorageSize([]string{filepath.Join(root, "disk.img"), filepath.Join(root, "aux.img"), filepath.Join(root, "config.json")}, s.limits.MaxExportBytes)
}
