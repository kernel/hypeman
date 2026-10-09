package builds

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type failingMachineSource struct{}

func (failingMachineSource) Read(p []byte) (int, error) {
	return copy(p, "partial"), errors.New("secret-source-error")
}

func TestMachineBuildSourceReadFailure(t *testing.T) {
	f := newMachineFixture()
	r := machineTestRunner(t, f)
	result, err := r.Run(context.Background(), machineTestRequest(), failingMachineSource{})
	if err == nil || result != nil || strings.Contains(err.Error(), "secret-source-error") {
		t.Fatal("source failure was not masked")
	}
	if !reflect.DeepEqual(f.calls, []string{"resolve"}) {
		t.Fatalf("source failure started guest: %v", f.calls)
	}
	entries, err := os.ReadDir(r.WorkDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("partial source retained")
	}
}

func TestMachineBuildTimeoutCleanup(t *testing.T) {
	f := newMachineFixture()
	f.waitForCancel = true
	r := machineTestRunner(t, f)
	req := machineTestRequest()
	req.Policy.TimeoutSeconds = 1
	parent, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := r.Run(parent, req, strings.NewReader("source"))
	if !errors.Is(err, context.DeadlineExceeded) || result != nil || f.publisherCalled || !f.cleanupContextLive {
		t.Fatal("timeout cleanup failed")
	}
	if !reflect.DeepEqual(f.calls, []string{"resolve", "start", "stop", "destroy"}) {
		t.Fatalf("timeout cleanup order: %v", f.calls)
	}
}

func TestMachineBuildPublicationReceipt(t *testing.T) {
	for _, kind := range []string{"ambiguous error", "tag receipt", "digest mismatch", "invalid digest"} {
		t.Run(kind, func(t *testing.T) {
			f := newMachineFixture()
			r := machineTestRunner(t, f)
			switch kind {
			case "ambiguous error":
				f.fail = "publish"
			case "tag receipt":
				f.publicationOverride = &MachinePublication{Reference: "localhost/builds/build-test:latest", Digest: machineTestDigest}
			case "digest mismatch":
				f.publicationOverride = &MachinePublication{Reference: "localhost/builds/build-test@" + machineTestDigest, Digest: "sha256:" + strings.Repeat("b", 64)}
			case "invalid digest":
				f.publicationOverride = &MachinePublication{Reference: "localhost/builds/build-test@" + machineTestDigest, Digest: "not-a-digest"}
			}
			result, err := r.Run(context.Background(), machineTestRequest(), strings.NewReader("source"))
			if err == nil || result != nil || !f.publisherCalled {
				t.Fatal("unverified receipt became ready")
			}
			if strings.Contains(err.Error(), "sensitive-command-argument") {
				t.Fatal("publisher error leaked")
			}
			count := 0
			for _, phase := range f.calls {
				if phase == "publish" {
					count++
				}
			}
			if count != 1 {
				t.Fatal("ambiguous publication was retried")
			}
		})
	}
}

func TestMachineBuildMalformedExport(t *testing.T) {
	for _, kind := range []string{"empty disk", "empty aux", "symlink disk", "hardlink payloads", "truncated config", "oversized config", "resource change", "bad MAC"} {
		t.Run(kind, func(t *testing.T) {
			f := newMachineFixture()
			r := machineTestRunner(t, f)
			f.mutateExport = func(root string) error {
				disk, aux, config := filepath.Join(root, "disk.img"), filepath.Join(root, "aux.img"), filepath.Join(root, "config.json")
				switch kind {
				case "empty disk":
					return os.Truncate(disk, 0)
				case "empty aux":
					return os.Truncate(aux, 0)
				case "symlink disk":
					if err := os.Remove(disk); err != nil {
						return err
					}
					return os.Symlink(aux, disk)
				case "hardlink payloads":
					if err := os.Remove(aux); err != nil {
						return err
					}
					return os.Link(disk, aux)
				case "truncated config":
					return os.WriteFile(config, []byte(`{"hardware_model":`), 0600)
				case "oversized config":
					return os.WriteFile(config, []byte(strings.Repeat(" ", 65<<10)), 0600)
				case "resource change":
					data, err := os.ReadFile(config)
					if err != nil {
						return err
					}
					return os.WriteFile(config, []byte(strings.Replace(string(data), `"cpus":4`, `"cpus":2`, 1)), 0600)
				case "bad MAC":
					data, err := os.ReadFile(config)
					if err != nil {
						return err
					}
					return os.WriteFile(config, []byte(strings.Replace(string(data), "02:00:00:00:00:01", "02:00:00:00:00:01:02:03", 1)), 0600)
				}
				return nil
			}
			result, err := r.Run(context.Background(), machineTestRequest(), strings.NewReader("source"))
			if err == nil || result != nil || f.publisherCalled {
				t.Fatal("malformed export published")
			}
		})
	}
}
