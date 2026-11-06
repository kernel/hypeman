//go:build darwin

package instances

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/require"
)

func TestVZForkFromRunningCorrectness(t *testing.T) {
	if runtime.GOARCH != "arm64" || !isMacOS14OrLater(t) {
		t.Skip("VZ running forks require Apple Silicon and macOS 14+")
	}
	ensureMkfsExt4Available(t)
	mgr, tmpDir := setupVZTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p := paths.New(tmpDir)
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	ref := integrationTestImageRef(t, "docker.io/library/nginx:alpine")
	img, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: ref})
	require.NoError(t, err)
	waitName := img.Name
	if img.Digest != "" {
		parsed, err := images.ParseNormalizedRef(img.Name)
		require.NoError(t, err)
		waitName = parsed.Repository() + "@" + img.Digest
	}
	require.NoError(t, imageManager.WaitForReady(ctx, waitName))
	require.NoError(t, system.NewManager(p).EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "vz-correctness-source",
		Image:          ref,
		Size:           2 * 1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeVZ,
		Cmd:            []string{"sh", "-ec", vzForkWorkload},
	})
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	sourceID := source.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, sourceID) })
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 60*time.Second))
	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, 30*time.Second)
	require.NoError(t, err)
	before := readVZForkWorkloadState(t, ctx, source)
	assertVZForkHTTP(t, ctx, source, "shared")

	fork, err := mgr.ForkInstance(ctx, sourceID, ForkInstanceRequest{
		Name:        "vz-correctness-fork",
		FromRunning: true,
		TargetState: StateRunning,
	})
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	forkID := fork.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, forkID) })
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 60*time.Second))
	require.NoError(t, waitForExecAgent(ctx, mgr, forkID, 60*time.Second))
	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, 30*time.Second)
	require.NoError(t, err)
	fork, err = waitForInstanceState(ctx, mgr, forkID, StateRunning, 30*time.Second)
	require.NoError(t, err)
	require.NotEqual(t, source.IP, fork.IP)
	require.NotEqual(t, source.MAC, fork.MAC)
	writeVZForkMarker(t, ctx, source, "source")
	writeVZForkMarker(t, ctx, fork, "fork")
	assertVZForkWorkload(t, ctx, source, fork, before)

	for i := 0; i < 2; i++ {
		beforeRestore := readVZForkWorkloadState(t, ctx, fork)
		_, err = mgr.StandbyInstance(ctx, forkID, StandbyInstanceRequest{})
		require.NoError(t, err)
		_, err = mgr.RestoreInstance(ctx, forkID)
		require.NoError(t, err)
		require.NoError(t, waitForExecAgent(ctx, mgr, forkID, 60*time.Second))
		fork, err = waitForInstanceState(ctx, mgr, forkID, StateRunning, 30*time.Second)
		require.NoError(t, err)
		afterRestore := readVZForkWorkloadState(t, ctx, fork)
		require.GreaterOrEqual(t, afterRestore.counter, beforeRestore.counter)
		assertVZForkWorkload(t, ctx, source, fork, before)
	}

	t.Run("host-http", func(t *testing.T) {
		if os.Getenv("HYPEMAN_VZ_FORK_HTTP") != "1" {
			t.Skip("set HYPEMAN_VZ_FORK_HTTP=1 to check forked NAT reachability")
		}
		assertVZForkHTTP(t, ctx, source, "source")
		assertVZForkHTTP(t, ctx, fork, "fork")
	})
}

// The shell counter and tmpfs nonce distinguish a RAM restore from a cold boot.
const vzForkWorkload = `
mkdir -p /dev/shm
mount -t tmpfs -o size=16m tmpfs /dev/shm
cat /proc/sys/kernel/random/uuid > /dev/shm/fork-nonce
echo $$ > /dev/shm/fork-pid
echo shared > /dev/shm/fork-marker
echo shared > /usr/share/nginx/html/index.html
nginx -g 'daemon off;' &
i=0
while :; do
  i=$((i + 1))
  echo "$i" > /dev/shm/fork-counter.tmp
  mv /dev/shm/fork-counter.tmp /dev/shm/fork-counter
  sleep 0.1
done
`

type vzForkWorkloadState struct {
	bootID  string
	pid     string
	nonce   string
	counter uint64
}

func readVZForkWorkloadState(t *testing.T, ctx context.Context, inst *Instance) vzForkWorkloadState {
	t.Helper()
	out, code, err := vzExecCommand(ctx, inst, "sh", "-ec",
		"cat /proc/sys/kernel/random/boot_id /dev/shm/fork-pid /dev/shm/fork-nonce /dev/shm/fork-counter")
	require.NoError(t, err)
	require.Equal(t, 0, code, out)
	fields := strings.Fields(out)
	require.Len(t, fields, 4)
	counter, err := strconv.ParseUint(fields[3], 10, 64)
	require.NoError(t, err)
	return vzForkWorkloadState{bootID: fields[0], pid: fields[1], nonce: fields[2], counter: counter}
}

func assertVZForkWorkload(t *testing.T, ctx context.Context, source, fork *Instance, before vzForkWorkloadState) {
	t.Helper()
	for _, inst := range []*Instance{source, fork} {
		state := readVZForkWorkloadState(t, ctx, inst)
		require.Equal(t, before.bootID, state.bootID, "guest must not cold-boot")
		require.Equal(t, before.pid, state.pid, "workload process must survive the fork")
		require.Equal(t, before.nonce, state.nonce, "tmpfs contents must survive the fork")
		require.GreaterOrEqual(t, state.counter, before.counter)
		require.Eventually(t, func() bool {
			out, code, err := vzExecCommand(ctx, inst, "cat", "/dev/shm/fork-counter")
			counter, parseErr := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
			return err == nil && code == 0 && parseErr == nil && counter > state.counter
		}, 10*time.Second, 100*time.Millisecond, "guest workload must continue making progress")
	}

	guests := []struct {
		inst   *Instance
		marker string
	}{
		{source, "source"},
		{fork, "fork"},
	}
	for _, item := range guests {
		out, code, err := vzExecCommand(ctx, item.inst, "sh", "-ec",
			"cat /dev/shm/fork-marker /usr/share/nginx/html/index.html")
		require.NoError(t, err)
		require.Equal(t, 0, code)
		require.Equal(t, []string{item.marker, item.marker}, strings.Fields(out), "guest RAM and disk writes must be independent")
	}

	// A smoke check for duplicated random streams, not a cryptographic audit.
	sourceRandom, code, err := vzExecCommand(ctx, source, "cat", "/proc/sys/kernel/random/uuid")
	require.NoError(t, err)
	require.Equal(t, 0, code)
	forkRandom, code, err := vzExecCommand(ctx, fork, "cat", "/proc/sys/kernel/random/uuid")
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.NotEmpty(t, strings.TrimSpace(sourceRandom))
	require.NotEmpty(t, strings.TrimSpace(forkRandom))
	require.NotEqual(t, sourceRandom, forkRandom, "fresh random output should diverge after the fork")
}

func writeVZForkMarker(t *testing.T, ctx context.Context, inst *Instance, marker string) {
	t.Helper()
	_, code, err := vzExecCommand(ctx, inst, "sh", "-ec",
		fmt.Sprintf("echo %s > /dev/shm/fork-marker; echo %s > /usr/share/nginx/html/index.html", marker, marker))
	require.NoError(t, err)
	require.Equal(t, 0, code)
}

func assertVZForkHTTP(t *testing.T, ctx context.Context, inst *Instance, marker string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	url := "http://" + net.JoinHostPort(inst.IP, "80") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if t.Failed() {
			diagCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, _, err := vzExecCommand(diagCtx, inst, "sh", "-c", "ip addr show eth0; ip route; wget -qO- http://127.0.0.1/")
			t.Logf("guest HTTP diagnostics: %s (exec error: %v)", out, err)
		}
	})
	require.Eventually(t, func() bool {
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return err == nil && resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == marker
	}, 20*time.Second, 200*time.Millisecond, "host HTTP request must reach the correct guest at %s", url)
}
