package builds

import (
	"context"
	"errors"
	"io"
	"os"
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

func TestMachineBuildMalformedStorage(t *testing.T) {
	for _, kind := range []string{"missing overlay", "empty aux", "symlink overlay", "aliased aux", "unrelated overlay", "raw overlay", "resource change", "bad MAC", "identity"} {
		t.Run(kind, func(t *testing.T) {
			f := newMachineFixture()
			f.mutateStorage = func(s *MachineStorage) error {
				switch kind {
				case "missing overlay":
					return os.Remove(s.Overlay)
				case "empty aux":
					return os.Truncate(s.Aux, 0)
				case "symlink overlay":
					if err := os.Rename(s.Overlay, s.Overlay+".real"); err != nil {
						return err
					}
					return os.Symlink(s.Overlay+".real", s.Overlay)
				case "aliased aux":
					if err := os.Remove(s.Aux); err != nil {
						return err
					}
					return os.Link(s.Overlay, s.Aux)
				case "unrelated overlay":
					other, err := machineStorageFixture(t.TempDir())
					if err != nil {
						return err
					}
					s.Overlay = other.Overlay
				case "raw overlay":
					return os.WriteFile(s.Overlay, []byte("not an asif image"), 0600)
				case "resource change":
					s.Platform.CPUs = 2
				case "bad MAC":
					s.Platform.MAC = "02:00:00:00:00:02:03:04"
				case "identity":
					s.Platform.MachineIdentifier = f.image.MacOS.MachineIdentifier // kept the base identity
				}
				return nil
			}
			_, err := runMachineBuildFailure(t, f, context.Background(), machineTestRequest(), strings.NewReader("source"))
			require.False(t, f.publisherCalled, "malformed storage published")
			require.Contains(t, err.Error(), "validate_storage")
		})
	}
}
