package builds

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func runMachineBuildFailure(t *testing.T, f *machineFixture, ctx context.Context, req MachineBuildRequest, source io.Reader) (*machineRunnerFixture, error) {
	t.Helper()
	runner := machineTestRunner(t, f)
	result, err := runner.Run(ctx, req, source)
	if err == nil || result != nil {
		t.Fatal("failed build returned a result")
	}
	return runner, err
}

type failingMachineSource struct{}

func (failingMachineSource) Read(p []byte) (int, error) {
	return copy(p, "partial"), errors.New("secret-source-error")
}

func TestMachineBuildSourceReadFailure(t *testing.T) {
	f := newMachineFixture()
	r, err := runMachineBuildFailure(t, f, context.Background(), machineTestRequest(), failingMachineSource{})
	if strings.Contains(err.Error(), "secret-source-error") {
		t.Fatal("source failure was not masked")
	}
	if len(f.calls) != 0 {
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
	req := machineTestRequest()
	req.Policy.TimeoutSeconds = 1
	parent, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := runMachineBuildFailure(t, f, parent, req, strings.NewReader("source"))
	if !errors.Is(err, context.DeadlineExceeded) || f.publisherCalled || !f.cleanupContextLive {
		t.Fatal("timeout cleanup failed")
	}
	if !reflect.DeepEqual(f.calls, []string{"start", "abort", "destroy"}) {
		t.Fatalf("timeout cleanup order: %v", f.calls)
	}
}

func TestMachineBuildPublicationReceipt(t *testing.T) {
	for _, kind := range []string{"ambiguous error", "tag receipt", "digest mismatch", "invalid digest"} {
		t.Run(kind, func(t *testing.T) {
			f := newMachineFixture()
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
			_, err := runMachineBuildFailure(t, f, context.Background(), machineTestRequest(), strings.NewReader("source"))
			if !f.publisherCalled {
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
	for _, kind := range []string{"empty disk", "empty aux", "symlink disk", "hardlink payloads", "truncated config", "oversized config", "resource change", "bad MAC", "identity", "extra file", "unknown metadata"} {
		t.Run(kind, func(t *testing.T) {
			f := newMachineFixture()
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
				case "identity", "unknown metadata":
					var raw map[string]any
					data, err := os.ReadFile(config)
					if err == nil {
						err = json.Unmarshal(data, &raw)
					}
					if err != nil {
						return err
					}
					if kind == "identity" {
						raw["machine_identifier"] = []byte{9}
					} else {
						raw["password"] = "must-not-publish"
					}
					data, _ = json.Marshal(raw)
					return os.WriteFile(config, data, 0600)
				case "extra file":
					return os.WriteFile(filepath.Join(root, "secret.txt"), []byte("must-not-publish"), 0600)
				}
				return nil
			}
			_, err := runMachineBuildFailure(t, f, context.Background(), machineTestRequest(), strings.NewReader("source"))
			if f.publisherCalled {
				t.Fatal("malformed export published")
			}
			if strings.Contains(err.Error(), "must-not-publish") {
				t.Fatal("export secret leaked in failure")
			}
		})
	}
}
