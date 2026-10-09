package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/images"
)

const machineTestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type machineFixture struct {
	image                                       *images.Image
	calls                                       []string
	fail                                        string
	stop                                        MachineStopReceipt
	cancel                                      context.CancelFunc
	waitForCancel                               bool
	mutateExport                                func(string) error
	publicationOverride                         *MachinePublication
	publisherCalled                             bool
	cleanupContextLive                          bool
	extraFile, changedIdentity, unknownMetadata bool
}

func newMachineFixture() *machineFixture {
	return &machineFixture{image: &images.Image{Digest: machineTestDigest, Status: images.StatusReady, Platform: "darwin/arm64", MacOS: &images.MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30}}, stop: MachineStopReceipt{Graceful: true, VMMExited: true}}
}
func (f *machineFixture) record(phase string) error {
	f.calls = append(f.calls, phase)
	if f.fail == phase {
		return errors.New("sensitive-command-argument")
	}
	return nil
}
func (f *machineFixture) ResolveBase(context.Context, string) (*images.Image, error) {
	if err := f.record("resolve"); err != nil {
		return nil, err
	}
	return f.image, nil
}
func (f *machineFixture) Start(_ context.Context, _ *images.Image, p BuildPolicy) (MachineBuildSession, error) {
	if p.CPUs != 4 || p.MemoryMB != 8192 {
		return nil, errors.New("wrong resource defaults")
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
func (f *machineFixture) Stop(ctx context.Context) (MachineStopReceipt, error) {
	f.cleanupContextLive = ctx.Err() == nil
	return f.stop, f.record("stop")
}
func (f *machineFixture) Export(_ context.Context, root string) error {
	if err := f.record("export"); err != nil {
		return err
	}
	platform := *f.image.MacOS
	if f.changedIdentity {
		platform.MachineIdentifier = []byte{9}
	}
	data, _ := json.Marshal(platform)
	if f.unknownMetadata {
		var raw map[string]any
		_ = json.Unmarshal(data, &raw)
		raw["password"] = "must-not-publish"
		data, _ = json.Marshal(raw)
	}
	for name, content := range map[string][]byte{"disk.img": []byte("disk"), "aux.img": []byte("aux"), "config.json": data} {
		if err := os.WriteFile(filepath.Join(root, name), content, 0666); err != nil {
			return err
		}
	}
	if f.extraFile {
		return os.WriteFile(filepath.Join(root, "secret.txt"), []byte("must-not-publish"), 0600)
	}
	if f.mutateExport != nil {
		return f.mutateExport(root)
	}
	return nil
}
func (f *machineFixture) Destroy(ctx context.Context) error {
	f.cleanupContextLive = ctx.Err() == nil
	return f.record("destroy")
}
func (f *machineFixture) Publish(ctx context.Context, id, root string) (MachinePublication, error) {
	f.publisherCalled = true
	if err := ctx.Err(); err != nil {
		return MachinePublication{}, err
	}
	if id != "build-test" {
		return MachinePublication{}, errors.New("wrong job namespace")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "source.tar.gz")); !os.IsNotExist(err) {
		return MachinePublication{}, errors.New("source remains during publish")
	}
	for _, name := range []string{"disk.img", "aux.img", "config.json"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.Mode().Perm() != 0600 {
			return MachinePublication{}, errors.New("unsafe export mode")
		}
	}
	publication := MachinePublication{Reference: "localhost/builds/build-test@" + machineTestDigest, Digest: machineTestDigest}
	if f.publicationOverride != nil {
		publication = *f.publicationOverride
	}
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
	expected := []string{"resolve", "start", "provision", "sanitize", "stop", "export", "destroy", "publish"}
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
	for _, phase := range []string{"resolve", "start", "provision", "sanitize", "stop", "export", "destroy"} {
		t.Run(phase, func(t *testing.T) {
			f := newMachineFixture()
			f.fail = phase
			r := machineTestRunner(t, f)
			result, err := r.Run(context.Background(), machineTestRequest(), strings.NewReader("source"))
			if err == nil || result != nil || f.publisherCalled {
				t.Fatal("failed build reached publication")
			}
			if strings.Contains(err.Error(), "sensitive-command-argument") {
				t.Fatal("raw guest error leaked")
			}
		})
	}
}

func TestMachineBuildStopReceipts(t *testing.T) {
	for _, receipt := range []MachineStopReceipt{{}, {VMMExited: true}} {
		f := newMachineFixture()
		f.stop = receipt
		r := machineTestRunner(t, f)
		_, err := r.Run(context.Background(), machineTestRequest(), strings.NewReader("source"))
		if err == nil || f.publisherCalled {
			t.Fatal("unconfirmed/forced stop published")
		}
		for _, phase := range f.calls {
			if phase == "export" {
				t.Fatal("unconfirmed/forced stop exported")
			}
			if phase == "destroy" && !receipt.VMMExited {
				t.Fatal("destroyed unconfirmed live storage")
			}
		}
	}
}

func TestMachineBuildCancellationUsesIndependentCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMachineFixture()
	f.cancel = cancel
	r := machineTestRunner(t, f)
	_, err := r.Run(ctx, machineTestRequest(), strings.NewReader("source"))
	if !errors.Is(err, context.Canceled) || !f.cleanupContextLive || f.publisherCalled {
		t.Fatal("cancellation did not retain safe cleanup")
	}
	if !reflect.DeepEqual(f.calls, []string{"resolve", "start", "stop", "destroy"}) {
		t.Fatalf("cancel cleanup: %v", f.calls)
	}
}

func TestMachineBuildInputAdmission(t *testing.T) {
	for _, kind := range []string{"floating base", "wrong digest", "linux", "pending", "shaping", "domains", "source hash", "oversized source", "CPU overflow", "memory precision"} {
		t.Run(kind, func(t *testing.T) {
			f := newMachineFixture()
			r := machineTestRunner(t, f)
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
			case "domains":
				req.Policy.AllowedDomains = []string{"example.com"}
			case "source hash":
				req.SourceHash = strings.Repeat("0", 64)
			case "oversized source":
				r.MaxSourceBytes = 3
			case "CPU overflow":
				f.image.MacOS.CPUs = ^uint(0)
			case "memory precision":
				f.image.MacOS.Memory++
			}
			_, err := r.Run(context.Background(), req, strings.NewReader("source"))
			if err == nil {
				t.Fatal("bad input accepted")
			}
			for _, phase := range f.calls {
				if phase == "start" {
					t.Fatal("bad input started a VM")
				}
			}
		})
	}
}

type cancelMachineSource struct{ cancel context.CancelFunc }

func (s cancelMachineSource) Read(p []byte) (int, error) {
	s.cancel()
	return copy(p, "source"), io.EOF
}
func TestMachineBuildCanceledSourceDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newMachineFixture()
	_, err := machineTestRunner(t, f).Run(ctx, machineTestRequest(), cancelMachineSource{cancel})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(f.calls, []string{"resolve"}) {
		t.Fatalf("canceled source started builder: %v", f.calls)
	}
}

func TestMachineBuildExportAdmission(t *testing.T) {
	for _, kind := range []string{"identity", "extra file", "unknown metadata"} {
		t.Run(kind, func(t *testing.T) {
			f := newMachineFixture()
			f.changedIdentity = kind == "identity"
			f.extraFile = kind == "extra file"
			f.unknownMetadata = kind == "unknown metadata"
			_, err := machineTestRunner(t, f).Run(context.Background(), machineTestRequest(), strings.NewReader("source"))
			if err == nil || f.publisherCalled {
				t.Fatal("unsafe export published")
			}
			if strings.Contains(err.Error(), "must-not-publish") {
				t.Fatal("export secret leaked in failure")
			}
		})
	}
}

var _ MachineBuildDriver = (*machineFixture)(nil)
var _ MachineBuildSession = (*machineFixture)(nil)
var _ MachineBuildPublisher = (*machineFixture)(nil)
