package instances

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

type macOSBootAdmissionStarter struct {
	concurrentForkPrepareTestStarter
	manager *manager
	started chan string
	release <-chan struct{}
}

func (s *macOSBootAdmissionStarter) StartVM(_ context.Context, _ *paths.Paths, _ string, socket string, _ hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	id := filepath.Base(filepath.Dir(socket))
	s.started <- id
	<-s.release
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		return 0, nil, err
	}
	s.manager.storeCachedHypervisorState(id, hypervisor.StateRunning)
	return os.Getpid(), lifecycleNoopHypervisor{state: hypervisor.StateRunning}, nil
}

func TestMacOSBootAdmissionSkipsOnlyForeignMetadata(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	guest := &StoredMetadata{Id: "guest", SocketPath: p.InstanceSocket("guest", "vz.sock"), MacOS: testMacImage().MacOS, NetworkEnabled: true}
	require.NoError(t, m.ensureDirectories("partial-copy"))
	path := p.InstanceMetadata("partial-copy")
	require.NoError(t, os.WriteFile(path, []byte(`{"Id":"old-source","MAC":"02:00:00:00:00:01"}`), 0644))
	require.NoError(t, m.checkMacOSBootAvailable(t.Context(), guest))
	macs, err := m.storedMACs(guest.Id)
	require.NoError(t, err)
	require.Empty(t, macs)

	for _, invalid := range []string{`{`, `{}`} {
		require.NoError(t, os.WriteFile(path, []byte(invalid), 0644))
		require.Error(t, m.checkMacOSBootAvailable(t.Context(), guest))
	}
}

func TestMacOSConcurrentBootsRecheckMACUnderBootLock(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	release := make(chan struct{})
	starter := &macOSBootAdmissionStarter{manager: m, started: make(chan string, 2), release: release}
	hvType := hypervisor.Type("macos-boot-admission-test")
	m.vmStarters = map[hypervisor.Type]hypervisor.VMStarter{hvType: starter}
	var guests []*StoredMetadata
	for i, id := range []string{"first", "second"} {
		require.NoError(t, m.ensureDirectories(id))
		macos := *testMacImage().MacOS
		macos.MachineIdentifier = []byte{byte(i + 1)}
		stored := &StoredMetadata{Id: id, DataDir: p.InstanceDir(id), SocketPath: p.InstanceSocket(id, starter.SocketName()), HypervisorType: hvType, MacOS: &macos, MAC: macos.MAC, NetworkEnabled: true, Size: int64(macos.Memory), Vcpus: int(macos.CPUs)}
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: *stored}))
		guests = append(guests, stored)
	}

	// Both advisory checks succeed before either guest enters the boot lock.
	for _, stored := range guests {
		require.NoError(t, m.checkMacOSMACAvailable(context.Background(), stored))
		require.NoError(t, m.checkMacOSIdentityAvailable(context.Background(), stored))
	}
	results := make(chan error, 2)
	for _, stored := range guests {
		go func() { results <- m.startAndBootVM(context.Background(), stored, nil, nil, false) }()
	}
	select {
	case <-starter.started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("first boot did not start")
	}
	close(release)
	var succeeded, refused int
	for range guests {
		select {
		case err := <-results:
			if err == nil {
				succeeded++
			} else {
				require.True(t, errors.Is(err, ErrInsufficientResources), "%v", err)
				refused++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("boot admission did not finish")
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, refused)
	require.Empty(t, starter.started, "the conflicting guest must never reach StartVM")
}
