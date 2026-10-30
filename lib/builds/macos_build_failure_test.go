package builds

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

type cancelMachineSource struct{ cancel context.CancelFunc }

func (s cancelMachineSource) Read(p []byte) (int, error) {
	s.cancel()
	return copy(p, "source"), io.EOF
}

func TestMachineBuildSourceFailureDoesNotStart(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     func(context.CancelFunc) io.Reader
		wantCancel bool
	}{
		{name: "read failure", source: func(context.CancelFunc) io.Reader { return failingMachineSource{} }},
		{name: "canceled", source: func(cancel context.CancelFunc) io.Reader { return cancelMachineSource{cancel} }, wantCancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newMachineFixture()
			r, err := runMachineBuildFailure(t, f, ctx, machineTestRequest(), tc.source(cancel))
			if tc.wantCancel {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NotContains(t, err.Error(), "secret-source-error", "source failure was not masked")
			}
			require.Empty(t, f.calls, "source failure started guest")
			entries, err := os.ReadDir(r.WorkDir)
			require.NoError(t, err)
			require.Empty(t, entries, "partial source retained")
		})
	}
}

// Cancellation and timeout both interrupt a running build. Cleanup must still
// run to completion on a live context, and nothing may publish.
func TestMachineBuildInterruptedCleanup(t *testing.T) {
	assertCleanup := func(t *testing.T, f *machineFixture, err, want error) {
		t.Helper()
		require.ErrorIs(t, err, want)
		require.False(t, f.publisherCalled, "interrupted build published")
		require.True(t, f.cleanupContextLive, "cleanup ran on a cancelled context")
		require.Equal(t, []string{"start", "abort", "destroy"}, f.calls)
	}
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f := newMachineFixture()
		f.cancel = cancel
		_, err := runMachineBuildFailure(t, f, ctx, machineTestRequest(), strings.NewReader("source"))
		assertCleanup(t, f, err, context.Canceled)
	})
	t.Run("timeout", func(t *testing.T) {
		f := newMachineFixture()
		f.waitForCancel = true
		req := machineTestRequest()
		req.Policy.TimeoutSeconds = 1
		parent, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := runMachineBuildFailure(t, f, parent, req, strings.NewReader("source"))
		assertCleanup(t, f, err, context.DeadlineExceeded)
	})
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
					return os.WriteFile(config, []byte(strings.Replace(string(data), "02:00:00:00:00:02", "02:00:00:00:00:02:03:04", 1)), 0600)
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
						raw["machine_identifier"] = f.image.MacOS.MachineIdentifier // kept the base identity
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
