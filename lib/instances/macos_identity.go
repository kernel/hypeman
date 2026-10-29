package instances

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
)

// macOSIdentityRekeyCommand replaces the guest's SSH host keys. A clone shares its
// source's keys until this runs, so it runs on the clone's first start.
const macOSIdentityRekeyCommand = "rm -f /etc/ssh/ssh_host_* && ssh-keygen -A"

const (
	macOSRekeyDeadline    = 3 * time.Minute
	macOSRekeyStderrLimit = 4 << 10
)

// boundedBuffer keeps at most limit bytes and reports every write as accepted, so a guest
// cannot grow host memory or stall the exec stream by writing more.
type boundedBuffer struct {
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return string(b.buf) }

// prepareMacOSClone gives a stopped macOS clone its own identity: a fresh Apple machine
// identifier and a pending SSH host key rekey. Its MAC is assigned under the fork
// admission lock. The macOS struct is copied so the clone never aliases its source.
func prepareMacOSClone(clone *StoredMetadata) error {
	if clone.MacOS == nil {
		return nil
	}
	if !clone.MacOS.GuestAgent || clone.SkipGuestAgent {
		return fmt.Errorf("%w: macOS clone needs the guest agent to rekey its identity", ErrInvalidRequest)
	}
	macos := *clone.MacOS
	macos.HardwareModel = append([]byte(nil), clone.MacOS.HardwareModel...)
	id, err := newMacMachineIdentifierData()
	if err != nil {
		return fmt.Errorf("generate macOS machine identifier: %w", err)
	}
	macos.MachineIdentifier = id
	clone.IP = ""
	clone.MacOS = &macos
	clone.IdentityRekeyPending = true
	return nil
}

// forEachOtherInstance calls fn with the metadata of every stored instance except
// exclude, stopping at fn's first error. Directories without metadata are skipped;
// any other load error is wrapped with loadErrorContext when it is set.
func (m *manager) forEachOtherInstance(exclude, loadErrorContext string, fn func(*metadata) error) error {
	entries, err := os.ReadDir(m.paths.GuestsDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == exclude {
			continue
		}
		meta, err := m.loadMetadata(entry.Name())
		if errors.Is(err, ErrNotFound) {
			// A create in progress or a leftover directory has no instance to admit against.
			continue
		}
		if err != nil {
			if loadErrorContext != "" {
				return fmt.Errorf("%s: %w", loadErrorContext, err)
			}
			return err
		}
		if err := fn(meta); err != nil {
			return err
		}
	}
	return nil
}

// storedMACs returns every MAC recorded by a stored instance, except exclude.
func (m *manager) storedMACs(exclude string) ([]string, error) {
	var macs []string
	err := m.forEachOtherInstance(exclude, "", func(meta *metadata) error {
		if meta.MAC != "" {
			macs = append(macs, meta.MAC)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return macs, nil
}

// checkMacOSMACAvailable refuses a macOS start whose MAC a live instance already holds.
// Two live guests on one MAC share one vmnet DHCP lease, so the API would report one
// address for both.
func (m *manager) checkMacOSMACAvailable(ctx context.Context, stored *StoredMetadata) error {
	mac := macOSBootMAC(stored)
	if mac == "" {
		return nil
	}
	return m.forEachOtherInstance(stored.Id, "check macOS MAC availability", func(meta *metadata) error {
		other := meta.MAC
		if meta.MacOS != nil {
			other = macOSBootMAC(&meta.StoredMetadata)
		}
		if !meta.NetworkEnabled || !sameMACAddress(other, mac) {
			return nil
		}
		state := m.deriveStateWithoutHydration(ctx, &meta.StoredMetadata).State
		if state.RequiresVMM() || state == StateUnknown {
			return fmt.Errorf("%w: MAC %s is already in use by instance %s; stop it first", ErrInsufficientResources, mac, meta.Id)
		}
		return nil
	})
}

// completeMacOSIdentityRekey runs on a clone's first start after the guest agent answers.
// It clears the pending flag only after the rekey succeeds and is saved, so a failure
// leaves the flag set and the next start retries.
func (m *manager) completeMacOSIdentityRekey(ctx context.Context, stored *StoredMetadata) error {
	log := logger.FromContext(ctx)
	dialer, err := hypervisor.NewVsockDialer(stored.HypervisorType, stored.VsockSocket, stored.VsockCID)
	if err != nil {
		return fmt.Errorf("create vsock dialer for identity rekey: %w", err)
	}
	// The rekey runs from the clone's own disk, which the source owner controls. Its output is
	// untrusted: stdout is discarded and stderr is kept only up to a small cap. The deadline
	// bounds the guest's work even if it stalls or streams without end.
	var stderr boundedBuffer
	stderr.limit = macOSRekeyStderrLimit
	rekeyCtx, cancel := context.WithTimeout(ctx, macOSRekeyDeadline)
	defer cancel()
	exit, err := guest.ExecIntoInstance(rekeyCtx, dialer, guest.ExecOptions{
		Command:      []string{"sh", "-c", macOSIdentityRekeyCommand},
		Stdout:       io.Discard,
		Stderr:       &stderr,
		Timeout:      int32(macOSRekeyDeadline / time.Second),
		WaitForAgent: 120 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("exec macOS identity rekey: %w", err)
	}
	if exit.Code != 0 {
		return fmt.Errorf("macOS identity rekey failed (exit=%d, stderr=%q)", exit.Code, stderr.String())
	}
	stored.IdentityRekeyPending = false
	if err := m.saveMetadata(&metadata{StoredMetadata: *stored}); err != nil {
		return fmt.Errorf("save macOS identity rekey: %w", err)
	}
	log.InfoContext(ctx, "macOS clone identity rekeyed", "instance_id", stored.Id)
	return nil
}

// rekeyMacOSCloneOnStart rekeys a macOS clone's guest SSH host keys on its first
// start. A failed rekey stops the VM, and the pending flag stays set so the next
// start retries.
func (m *manager) rekeyMacOSCloneOnStart(ctx context.Context, stored *StoredMetadata) error {
	if stored.MacOS == nil || !stored.IdentityRekeyPending {
		return nil
	}
	err := m.completeMacOSIdentityRekey(ctx, stored)
	if err == nil {
		return nil
	}
	logger.FromContext(ctx).ErrorContext(ctx, "macOS identity rekey failed; stopping clone", "instance_id", stored.Id, "error", err)
	// Teardown runs even if saving the pending flag fails. Otherwise the clone would keep
	// running, networked, with its source's SSH host keys.
	saveErr := m.saveMetadata(&metadata{StoredMetadata: *stored})
	_, stopErr := m.stopInstance(context.WithoutCancel(ctx), stored.Id, false)
	if saveErr != nil || stopErr != nil {
		return fmt.Errorf("macOS clone rekey failed and teardown needs attention: %w", errors.Join(err, saveErr, stopErr))
	}
	return err
}

// checkMacOSBootAvailable checks machine identity and MAC under macOSBootMu.
// A stopped or standby guest does not reserve either against another boot.
func (m *manager) checkMacOSBootAvailable(ctx context.Context, stored *StoredMetadata) error {
	if err := m.checkMacOSIdentityAvailable(ctx, stored); err != nil {
		return err
	}
	if !stored.NetworkEnabled {
		return nil
	}
	return m.checkMacOSMACAvailable(ctx, stored)
}

// clearMacOSStopReceipt persists the end of the last graceful stop before the guest
// boots and can write to its disk. Otherwise a start that fails after boot, or a host
// crash, would leave a receipt that admits a stopped snapshot of a changed disk.
func (m *manager) clearMacOSStopReceipt(stored *StoredMetadata) error {
	stored.StopGraceful = false
	meta, err := m.loadMetadata(stored.Id)
	if err != nil {
		return err
	}
	if !meta.StopGraceful {
		return nil
	}
	meta.StopGraceful = false
	if err := m.saveMetadata(meta); err != nil {
		return fmt.Errorf("clear macOS stop receipt: %w", err)
	}
	return nil
}

// freshMacOSIdentity gives a new macOS instance its own machine identifier and MAC
// instead of its image's, so it can run beside the image's other instances. The
// guest's SSH host keys are rekeyed on first start.
func (m *manager) freshMacOSIdentity(stored *StoredMetadata) error {
	if stored.MacOS == nil {
		return nil
	}
	if err := prepareMacOSClone(stored); err != nil {
		return err
	}
	used, err := m.storedMACs(stored.Id)
	if err != nil {
		return fmt.Errorf("list MACs for macOS identity: %w", err)
	}
	mac, err := network.NewUniqueMAC(used)
	if err != nil {
		return fmt.Errorf("allocate macOS MAC: %w", err)
	}
	stored.MacOS.MAC = mac
	stored.MAC = mac
	return nil
}

// assignMacOSCloneMAC gives a networked clone a MAC no stored instance uses. It runs
// under the fork admission lock, so concurrent forks cannot pick the same MAC.
func (m *manager) assignMacOSCloneMAC(clone *StoredMetadata) error {
	if clone.MacOS == nil || !clone.NetworkEnabled {
		return nil
	}
	used, err := m.storedMACs(clone.Id)
	if err != nil {
		return fmt.Errorf("list MACs for macOS clone: %w", err)
	}
	mac, err := network.NewUniqueMAC(used)
	if err != nil {
		return fmt.Errorf("allocate macOS clone MAC: %w", err)
	}
	macos := *clone.MacOS
	macos.MAC = mac
	clone.MacOS = &macos
	clone.MAC = mac
	return nil
}
