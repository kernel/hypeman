package builds

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func testMachineStorage(t *testing.T) *MachineBuildStorage {
	t.Helper()
	s, err := NewMachineBuildStorage(t.TempDir(), MachineBuildLimits{MaxConcurrent: 2, MaxExportBytes: 8 << 20, MinFreeBytes: 1 << 20})
	require.NoError(t, err)
	return s
}

func TestMachineStorageLimits(t *testing.T) {
	for _, limits := range []MachineBuildLimits{{}, {1, 1, 1}, {0, 8 << 20, 1}, {1, 8 << 20, 0}, {1, math.MaxInt64, 1}, {1, 8 << 20, math.MaxInt64}} {
		require.Error(t, limits.Validate())
	}
	_, err := NewMachineBuildStorage("relative", MachineBuildLimits{1, 8 << 20, 1})
	require.Error(t, err)
}

func TestMachineStorageAdmission(t *testing.T) {
	s := testMachineStorage(t)
	perJob := 4*s.limits.MaxExportBytes + (256 << 20)
	free := s.limits.MinFreeBytes + 2*perJob
	s.available = func(string) (int64, error) { return free, nil }
	ctx := context.Background()
	require.NoError(t, s.reserve(ctx, "one", s.root))
	require.Error(t, s.reserve(ctx, "one", s.root))
	free--
	require.Error(t, s.reserve(ctx, "two", s.root), "must account for another job's future writes")
	free++
	require.NoError(t, s.reserve(ctx, "two", s.root))
	require.Error(t, s.reserve(ctx, "three", s.root))
	require.NoError(t, s.finish("one", false))
	require.NoError(t, s.reaped("two"))
	require.Len(t, s.jobs, 2, "recovery cannot release an active job")
	require.NoError(t, s.reaped("one"))
	require.NoError(t, s.finish("two", true))
	require.Empty(t, s.jobs)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, s.reserve(cancelled, "cancelled", s.root), context.Canceled)
	s.available = func(string) (int64, error) { return 0, errors.New("statfs failed") }
	require.Error(t, s.reserve(ctx, "unknown", s.root))
	require.Empty(t, s.jobs)
}

func TestMachineStorageConcurrentAdmission(t *testing.T) {
	s := testMachineStorage(t)
	s.available = func(string) (int64, error) { return math.MaxInt64, nil }
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if s.reserve(context.Background(), strconv.Itoa(i), s.root) == nil {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 2, admitted.Load())
}

func TestMachineStorageRestartRetainsReservations(t *testing.T) {
	s := testMachineStorage(t)
	s.available = func(string) (int64, error) { return math.MaxInt64, nil }
	require.NoError(t, s.reserve(context.Background(), "interrupted", s.root))
	restarted, err := NewMachineBuildStorage(s.root, s.limits)
	require.NoError(t, err)
	require.True(t, restarted.jobs["interrupted"].quarantined)
	changed := s.limits
	changed.MaxExportBytes /= 2
	_, err = NewMachineBuildStorage(s.root, changed)
	require.Error(t, err, "cannot silently shrink an interrupted job's reservation")
	require.Error(t, restarted.reserve(context.Background(), "interrupted", s.root))
	require.NoError(t, restarted.reaped("interrupted"))
	require.NoError(t, restarted.reserve(context.Background(), "interrupted", s.root))
	require.NoError(t, restarted.finish("interrupted", true))
	restarted, err = NewMachineBuildStorage(s.root, s.limits)
	require.NoError(t, err)
	require.Empty(t, restarted.jobs)
}

func TestMachineReservationAtomicMarkers(t *testing.T) {
	s := testMachineStorage(t)
	// A process may exit while writing its unpublished temporary marker.
	require.NoError(t, os.WriteFile(filepath.Join(s.markers, ".reservation-interrupted"), []byte("8"), 0600))
	restarted, err := NewMachineBuildStorage(s.root, s.limits)
	require.NoError(t, err)
	require.Empty(t, restarted.jobs)
	require.NoError(t, restarted.reserve(context.Background(), "job", restarted.root))
	marker := filepath.Join(s.markers, "job")
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(s.limits.MaxExportBytes, 10), string(data))
	require.Error(t, restarted.writeMarker("job", "partial", false), "initial marker cannot overwrite an existing reservation")
	data, err = os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(s.limits.MaxExportBytes, 10), string(data))
	require.NoError(t, restarted.RecordPublication(context.Background(), "job", machineTestDigest))
	data, err = os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(s.limits.MaxExportBytes, 10)+"\n"+machineTestDigest, string(data))
}

func TestMachinePublicationDigestSurvivesRestart(t *testing.T) {
	s := testMachineStorage(t)
	require.NoError(t, s.reserve(context.Background(), "job", s.root))
	require.Error(t, s.RecordPublication(context.Background(), "job", "invalid"))
	require.NoError(t, s.RecordPublication(context.Background(), "job", machineTestDigest))
	require.NoError(t, s.finish("job", false))
	restarted, err := NewMachineBuildStorage(s.root, s.limits)
	require.NoError(t, err)
	require.Equal(t, machineTestDigest, restarted.publicationDigest("job"))
	require.True(t, restarted.quarantined("job"))
}

type delayedPublicationImages struct {
	images.Manager
	expected string
	ready    atomic.Bool
	lookedUp chan string
	latest   atomic.Int32
}

func (i *delayedPublicationImages) GetImage(_ context.Context, ref string) (*images.Image, error) {
	select {
	case i.lookedUp <- ref:
	default:
	}
	if !strings.Contains(ref, "@") {
		i.latest.Add(1)
		return &images.Image{Status: images.StatusReady, Digest: "sha256:" + strings.Repeat("b", 64)}, nil
	}
	if ref != i.expected || !i.ready.Load() {
		return nil, images.ErrNotFound
	}
	return &images.Image{Status: images.StatusReady, Digest: machineTestDigest}, nil
}

func TestMachinePublicationWaitsForExactDelayedImport(t *testing.T) {
	f := newMachineFixture()
	m := newMachineTestManager(t, f, f, nil, false)
	imageManager := &delayedPublicationImages{expected: "builds/job@" + machineTestDigest, lookedUp: make(chan string, 1)}
	m.imageManager = imageManager
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.waitMachinePublication(ctx, "job", machineTestDigest) }()
	select {
	case ref := <-imageManager.lookedUp:
		require.Equal(t, imageManager.expected, ref)
	case <-ctx.Done():
		t.Fatal("no image lookup")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before async conversion started: %v", err)
	default:
	}
	imageManager.ready.Store(true)
	require.NoError(t, <-done)
	require.Zero(t, imageManager.latest.Load(), "stale latest must never satisfy publication")
	imageManager.ready.Store(false)
	bounded, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	require.ErrorIs(t, m.waitMachinePublication(bounded, "job", machineTestDigest), context.DeadlineExceeded)
}

func TestMachineReconcileRetainsUncreatedPublication(t *testing.T) {
	f := newMachineFixture()
	m := newMachineTestManager(t, f, f, nil, false)
	s := m.config.MachineBuild.Storage
	require.NoError(t, s.reserve(context.Background(), "job", s.root))
	require.NoError(t, s.RecordPublication(context.Background(), "job", machineTestDigest))
	require.NoError(t, s.finish("job", false))
	require.NoError(t, writeMetadata(m.paths, &buildMetadata{ID: "job", Status: StatusCancelled}))
	imageManager := &delayedPublicationImages{expected: "builds/job@" + machineTestDigest, lookedUp: make(chan string, 1)}
	m.imageManager = imageManager
	require.NoError(t, m.reconcileMachineStorage(context.Background()))
	require.True(t, s.quarantined("job"), "NotFound is not terminal after manifest submission")
	imageManager.ready.Store(true)
	require.NoError(t, m.reconcileMachineStorage(context.Background()))
	require.Empty(t, s.jobs)
	require.Zero(t, imageManager.latest.Load())
	// Without a recorded digest the production publisher could not have committed.
	require.NoError(t, s.reserve(context.Background(), "job", s.root))
	require.NoError(t, s.finish("job", false))
	imageManager.ready.Store(false)
	require.NoError(t, m.reconcileMachineStorage(context.Background()))
	require.Empty(t, s.jobs)
}

func TestMachineStorageSourceHeadroom(t *testing.T) {
	s := testMachineStorage(t)
	free := s.limits.MinFreeBytes + 10
	s.available = func(string) (int64, error) { return free, nil }
	called := false
	stage := func() error { called = true; return nil }
	path := filepath.Join(s.root, "source")
	require.Error(t, s.stageSource(path, 11, stage))
	require.False(t, called)
	require.NoError(t, s.stageSource(path, 10, stage))
	require.True(t, called)
	free = math.MaxInt64
	require.NoError(t, s.reserve(context.Background(), "active", s.root))
	free = s.limits.MinFreeBytes + 4*s.limits.MaxExportBytes + (256 << 20)
	require.NoError(t, s.checkHeadroom())
	require.Error(t, s.stageSource(path, 1, stage))
	free--
	require.Error(t, s.checkHeadroom())
}

func TestMachineBuildCapacityCleanup(t *testing.T) {
	for _, mode := range []string{"cancel", "oversize", "destroy-failure", "unconfirmed-exit", "no-headroom", "unconfigured"} {
		t.Run(mode, func(t *testing.T) {
			f := newMachineFixture()
			r := machineTestRunner(t, f)
			storage := r.Storage
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "cancel":
				f.cancel = cancel
			case "oversize":
				f.mutateExport = func(root string) error {
					return os.Truncate(filepath.Join(root, "disk.img"), storage.limits.MaxExportBytes+1)
				}
			case "destroy-failure":
				f.fail = "destroy"
			case "unconfirmed-exit":
				f.stop = MachineStopReceipt{}
			case "no-headroom":
				storage.available = func(string) (int64, error) { return 0, nil }
			case "unconfigured":
				r.Storage = nil
			}
			_, err := r.Run(ctx, machineTestRequest(), strings.NewReader("source"))
			require.Error(t, err)
			require.False(t, f.publisherCalled)
			if mode == "destroy-failure" || mode == "unconfirmed-exit" {
				require.True(t, storage.jobs["build-test"].quarantined)
				restarted, err := NewMachineBuildStorage(storage.root, storage.limits)
				require.NoError(t, err)
				require.Len(t, restarted.jobs, 1)
			} else {
				require.Empty(t, storage.jobs)
			}
			if mode == "no-headroom" || mode == "unconfigured" {
				require.NotContains(t, f.calls, "start")
			}
		})
	}
}

type capacityImages struct {
	images.Manager
	status string
}

func (i capacityImages) GetImage(context.Context, string) (*images.Image, error) {
	return &images.Image{Status: i.status, Digest: machineTestDigest}, nil
}

func TestMachineStorageTerminalRecovery(t *testing.T) {
	for _, mode := range []string{"terminal", "live-vm", "importing", "workspace"} {
		t.Run(mode, func(t *testing.T) {
			f := newMachineFixture()
			m := newMachineTestManager(t, f, f, nil, false)
			s := m.config.MachineBuild.Storage
			require.NoError(t, s.reserve(context.Background(), "terminal", s.root))
			require.NoError(t, s.RecordPublication(context.Background(), "terminal", machineTestDigest))
			require.NoError(t, s.finish("terminal", false))
			require.NoError(t, writeMetadata(m.paths, &buildMetadata{ID: "terminal", Status: StatusCancelled}))
			m.instanceManager = &machineLifecycleInstances{missing: mode != "live-vm"}
			status := images.StatusReady
			if mode == "importing" {
				status = images.StatusPending
			}
			m.imageManager = capacityImages{status: status}
			if mode == "workspace" {
				require.NoError(t, os.MkdirAll(filepath.Join(m.paths.BuildDir("terminal"), "machine-build-leftover"), 0700))
			}
			require.NoError(t, m.reconcileMachineStorage(context.Background()))
			if mode == "terminal" {
				require.Empty(t, s.jobs)
			} else {
				require.Len(t, s.jobs, 1)
			}
		})
	}
}

func TestMachineStorageHeldThroughImageImport(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			f := newMachineFixture()
			r := machineTestRunner(t, f)
			req := machineTestRequest()
			req.onPublished = func(_ context.Context, publication MachinePublication) error {
				require.Equal(t, machineTestDigest, publication.Digest)
				require.Len(t, r.Storage.jobs, 1)
				if fail {
					return context.DeadlineExceeded
				}
				return nil
			}
			_, err := r.Run(context.Background(), req, strings.NewReader("source"))
			if fail {
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.True(t, r.Storage.quarantined(req.ID))
			} else {
				require.NoError(t, err)
				require.Empty(t, r.Storage.jobs)
			}
		})
	}
}

type capacityExporter struct{ called bool }

func (*capacityExporter) StopMacOSBuildInstance(context.Context, string) (MachineStopReceipt, error) {
	return MachineStopReceipt{}, nil
}
func (e *capacityExporter) ExportStoppedMacOSBuildInstance(context.Context, string, string) error {
	e.called = true
	return nil
}

func TestGuestMachineExportChecksSizeBeforeCopy(t *testing.T) {
	s := testMachineStorage(t)
	p := paths.New(s.root)
	id := "builder"
	dir := p.InstanceDir(id)
	require.NoError(t, os.MkdirAll(dir, 0700))
	disk := p.InstanceMacOSBootDisk(id)
	require.NoError(t, os.WriteFile(disk, []byte{1}, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mac-aux.img"), []byte{1}, 0600))
	exporter := &capacityExporter{}
	session := &guestMachineSession{storage: s, paths: p, exporter: exporter, receipt: MachineStopReceipt{Graceful: true, VMMExited: true}, sanitized: true, instance: &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: id, DataDir: dir}}}
	destination := t.TempDir()
	require.NoError(t, session.Export(context.Background(), destination))
	require.True(t, exporter.called)
	exporter.called = false
	require.NoError(t, os.Truncate(disk, s.limits.MaxExportBytes))
	require.Error(t, session.Export(context.Background(), destination))
	require.False(t, exporter.called, "reject oversized raw disk before starting copy")
}

func TestMachineStorageFilesystem(t *testing.T) {
	s := testMachineStorage(t)
	free, err := machineAvailableBytes(s.root)
	require.NoError(t, err)
	require.Positive(t, free)
	require.NoError(t, s.sameFilesystem(s.root))
	require.Error(t, s.sameFilesystem(filepath.Join(s.root, "missing")))
	var rootFS, otherFS unix.Statfs_t
	require.NoError(t, unix.Statfs(s.root, &rootFS))
	if unix.Statfs("/dev/shm", &otherFS) == nil && rootFS.Fsid != otherFS.Fsid {
		require.Error(t, s.sameFilesystem("/dev/shm"))
	}
}

func TestMachineStorageSizeUsesLogicalBytes(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "disk")
	f, err := os.Create(file)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(8<<20))
	require.NoError(t, f.Close())
	require.NoError(t, machineStorageSize([]string{file}, 8<<20))
	require.Error(t, machineStorageSize([]string{file}, (8<<20)-1))
	aux := filepath.Join(root, "aux")
	require.NoError(t, os.WriteFile(aux, []byte{1}, 0600))
	require.Error(t, machineStorageSize([]string{file, aux}, 8<<20))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(file, link))
	require.Error(t, machineStorageSize([]string{link}, 8<<20))
	require.Error(t, machineStorageSize([]string{root}, 8<<20))
	require.Error(t, machineStorageSize([]string{filepath.Join(root, "missing")}, 8<<20))
}
