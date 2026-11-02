package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Code-Hex/vz/v4/diskimagekit"
	"github.com/kernel/hypeman/lib/images"
)

const machineTestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type machineFixture struct {
	mu                 sync.Mutex
	image              *images.Image
	calls              []string
	fail               string
	stop               MachineStopReceipt
	cancel             context.CancelFunc
	waitForCancel      bool
	mutateStorage      func(*MachineStorage) error
	storageDir         string
	publisherCalled    bool
	cleanupContextLive bool
	startGate          chan struct{}
}

func newMachineFixture() *machineFixture {
	return &machineFixture{image: &images.Image{Digest: machineTestDigest, Status: images.StatusReady, Platform: "darwin/arm64", MacOS: &images.MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30, GuestAgent: true}}, stop: MachineStopReceipt{Graceful: true, VMMExited: true}}
}
func (f *machineFixture) record(phase string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, phase)
	if f.fail == phase {
		return errors.New("sensitive-command-argument")
	}
	return nil
}
func (f *machineFixture) Start(_ context.Context, _ string, _ *images.Image, p BuildPolicy) (MachineBuildSession, error) {
	if p.CPUs != 4 || p.MemoryMB != 8192 {
		return nil, errors.New("wrong resource defaults")
	}
	if f.startGate != nil {
		<-f.startGate
	}
	return f, f.record("start")
}
func (f *machineFixture) ID() string { return "isolated-test-instance" }
func (f *machineFixture) Provision(ctx context.Context, source string) error {
	if _, err := os.ReadFile(source); err != nil {
		return err
	}
	if f.cancel != nil {
		f.cancel()
		return ctx.Err()
	}
	if f.waitForCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.record("provision")
}
func (f *machineFixture) Sanitize(context.Context) error { return f.record("sanitize") }
func (f *machineFixture) Stop(ctx context.Context, force bool) (MachineStopReceipt, error) {
	f.cleanupContextLive = ctx.Err() == nil
	phase := "stop"
	if force {
		phase = "abort"
	}
	return f.stop, f.record(phase)
}
func (f *machineFixture) Storage(context.Context) (MachineStorage, error) {
	if err := f.record("storage"); err != nil {
		return MachineStorage{}, err
	}
	dir, err := os.MkdirTemp("", "machine-storage-")
	if err != nil {
		return MachineStorage{}, err
	}
	f.storageDir = dir
	storage, err := machineStorageFixture(dir)
	if err != nil {
		return MachineStorage{}, err
	}
	storage.Platform = *f.image.MacOS
	// The builder has its own identity; its storage carries it, not the base's.
	storage.Platform.MachineIdentifier, storage.Platform.MAC = []byte{3}, "02:00:00:00:00:02"
	if f.mutateStorage != nil {
		if err := f.mutateStorage(&storage); err != nil {
			return MachineStorage{}, err
		}
	}
	return storage, nil
}

// machineStorageFixture writes a one-layer ASIF chain, its writable overlay and an
// aux file to dir, as a stopped builder has. The platform is left to the caller.
func machineStorageFixture(dir string) (MachineStorage, error) {
	storage := MachineStorage{Layers: []string{filepath.Join(dir, "disk.0.asif")}, Overlay: filepath.Join(dir, "boot.asif"), Aux: filepath.Join(dir, "mac-aux.img")}
	img, err := diskimagekit.CreateASIF(storage.Layers[0], (4<<20)/512)
	if err != nil {
		return storage, err
	}
	defer img.Close()
	if _, err := img.WriteAt([]byte("base"), 0); err != nil {
		return storage, err
	}
	if err := img.CreateLayer(storage.Overlay, diskimagekit.Overlay); err != nil {
		return storage, err
	}
	if _, err := img.WriteAt([]byte("build output"), 1<<20); err != nil {
		return storage, err
	}
	return storage, os.WriteFile(storage.Aux, []byte("aux"), 0600)
}
func (f *machineFixture) Destroy(ctx context.Context) error {
	f.cleanupContextLive = ctx.Err() == nil
	if f.storageDir != "" {
		os.RemoveAll(f.storageDir)
	}
	return f.record("destroy")
}
func (f *machineFixture) Publish(ctx context.Context, id string, storage MachineStorage, _ *MachineBase) (MachinePublication, error) {
	f.publisherCalled = true
	if err := ctx.Err(); err != nil {
		return MachinePublication{}, err
	}
	if id != "build-test" {
		return MachinePublication{}, errors.New("wrong job namespace")
	}
	for _, path := range append([]string{storage.Overlay, storage.Aux}, storage.Layers...) {
		if _, err := os.Stat(path); err != nil {
			return MachinePublication{}, errors.New("storage missing at publish")
		}
	}
	publication := MachinePublication{Reference: "localhost/builds/build-test@" + machineTestDigest, Digest: machineTestDigest}
	return publication, f.record("publish")
}
func machineTestRunner(t *testing.T, f *machineFixture) *machineRunnerFixture {
	t.Helper()
	return &machineRunnerFixture{MachineBuildBackend: MachineBuildBackend{Driver: f, Publisher: f}, WorkDir: t.TempDir()}
}
func machineTestRequest() MachineBuildRequest {
	return MachineBuildRequest{ID: "build-test", BaseImage: "localhost/macos@" + machineTestDigest}
}

func TestMachineBuildPublicationOrder(t *testing.T) {
	f := newMachineFixture()
	r := machineTestRunner(t, f)
	result, err := r.Run(context.Background(), machineTestRequest(), strings.NewReader("source"))
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"start", "provision", "sanitize", "stop", "storage", "publish", "destroy"}
	if !reflect.DeepEqual(f.calls, expected) {
		t.Fatalf("wrong phase order: %v", f.calls)
	}
	hash := sha256.Sum256([]byte("source"))
	if result.Provenance.SourceHash != hex.EncodeToString(hash[:]) || result.Provenance.BaseImageDigest != machineTestDigest {
		t.Fatal("unverified provenance")
	}
	entries, err := os.ReadDir(r.WorkDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("workspace not cleaned")
	}
}

func TestMachineBuildFailuresDoNotPublish(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   string
		receipt *MachineStopReceipt
	}{
		{name: "start", phase: "start"},
		{name: "provision", phase: "provision"},
		{name: "sanitize", phase: "sanitize"},
		{name: "stop", phase: "stop"},
		{name: "storage", phase: "storage"},
		{name: "unconfirmed stop", receipt: &MachineStopReceipt{}},
		{name: "forced stop", receipt: &MachineStopReceipt{VMMExited: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMachineFixture()
			f.fail = tc.phase
			if tc.receipt != nil {
				f.stop = *tc.receipt
			}
			_, err := runMachineBuildFailure(t, f, context.Background(), machineTestRequest(), strings.NewReader("source"))
			if f.publisherCalled {
				t.Fatal("failed build reached publication")
			}
			if tc.phase != "" && strings.Contains(err.Error(), "sensitive-command-argument") {
				t.Fatal("raw guest error leaked")
			}
			for _, phase := range f.calls {
				if phase == "storage" && tc.receipt != nil {
					t.Fatal("unconfirmed stop reached storage")
				}
				if phase == "destroy" && tc.receipt != nil && !tc.receipt.VMMExited {
					t.Fatal("destroyed unconfirmed live storage")
				}
			}
		})
	}
}

func TestMachineBuildInputAdmission(t *testing.T) {
	for _, kind := range []string{"floating base", "wrong digest", "linux", "pending", "shaping", "source hash", "CPU overflow", "memory precision"} {
		t.Run(kind, func(t *testing.T) {
			f := newMachineFixture()
			req := machineTestRequest()
			switch kind {
			case "floating base":
				req.BaseImage = "localhost/macos:latest"
			case "wrong digest":
				f.image.Digest = "sha256:" + strings.Repeat("b", 64)
			case "linux":
				f.image.Platform = "linux/arm64"
			case "pending":
				f.image.Status = images.StatusPending
			case "shaping":
				req.Policy.MemoryMB = 4096
			case "source hash":
				req.SourceHash = strings.Repeat("0", 64)
			case "CPU overflow":
				f.image.MacOS.CPUs = ^uint(0)
			case "memory precision":
				f.image.MacOS.Memory++
			}
			runMachineBuildFailure(t, f, context.Background(), req, strings.NewReader("source"))
			for _, phase := range f.calls {
				if phase == "start" {
					t.Fatal("bad input started a VM")
				}
			}
		})
	}
}

var _ MachineBuildDriver = (*machineFixture)(nil)
var _ MachineBuildSession = (*machineFixture)(nil)
var _ MachineBuildPublisher = (*machineFixture)(nil)
