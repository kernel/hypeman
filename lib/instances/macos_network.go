package instances

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"
)

// Each networked macOS guest gets its own vmnet network: a /29 from the configured
// pool, with the host at its first address and the guest at its second. vmnet's DHCP
// leaves the router out of its replies on a /30, so the guest would have no default
// route. The guest's
// address is stored with the instance and kept through stop and standby, so it never
// depends on vmnet's DHCP leases.
const (
	macOSGuestSubnetBits = 29
	macOSGuestNetmask    = "255.255.255.248"
)

// DefaultMacOSGuestSubnet is the pool macOS guest networks come from by default.
var DefaultMacOSGuestSubnet = netip.MustParsePrefix("192.168.128.0/20")

// macOSAddressReuseHold keeps a released guest address unassigned for longer than
// ingress caches upstream addresses (one minute), so a request meant for a deleted
// guest cannot reach the next guest given its address.
const macOSAddressReuseHold = 5 * time.Minute

const macOSAddressReleasesFile = "macos-address-releases.json"

// macOSGuestSubnet returns the /29 a macOS guest address belongs to.
func macOSGuestSubnet(ip string) (netip.Prefix, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return netip.Prefix{}, fmt.Errorf("invalid macOS guest address %q", ip)
	}
	return netip.PrefixFrom(addr, macOSGuestSubnetBits).Masked(), nil
}

// assignMacOSAddress gives a networked macOS guest without an address the first free
// guest address in the pool. A guest that has one keeps it. Addresses held by other
// instances, by creates in progress, or released within the reuse hold are skipped.
func (m *manager) assignMacOSAddress(stored *StoredMetadata) error {
	if stored.MacOS == nil || !stored.NetworkEnabled || stored.IP != "" {
		return nil
	}
	m.macOSAddressMu.Lock()
	defer m.macOSAddressMu.Unlock()
	unavailable, err := m.unavailableMacOSAddresses(stored.Id)
	if err != nil {
		return err
	}

	pool := m.macOSGuestSubnet
	if !pool.IsValid() {
		pool = DefaultMacOSGuestSubnet
	}
	size := uint64(1) << (32 - macOSGuestSubnetBits)
	base := pool.Masked().Addr().As4()
	start := uint64(base[0])<<24 | uint64(base[1])<<16 | uint64(base[2])<<8 | uint64(base[3])
	count := (uint64(1) << (32 - pool.Bits())) / size
	for i := uint64(0); i < count; i++ {
		v := start + i*size + 2
		guest := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		if unavailable[guest] {
			continue
		}
		stored.IP = guest.String()
		if m.macOSPendingAddresses == nil {
			m.macOSPendingAddresses = make(map[string]netip.Addr)
		}
		m.macOSPendingAddresses[stored.Id] = guest
		return nil
	}
	return fmt.Errorf("%w: no free macOS guest network in %s", ErrInsufficientResources, pool)
}

// macOSAddressAvailable reports whether id may use ip: no other instance or create
// holds it and it is not in its reuse hold.
func (m *manager) macOSAddressAvailable(id, ip string) (bool, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false, nil
	}
	m.macOSAddressMu.Lock()
	defer m.macOSAddressMu.Unlock()
	unavailable, err := m.unavailableMacOSAddresses(id)
	if err != nil {
		return false, err
	}
	return !unavailable[addr], nil
}

// unavailableMacOSAddresses returns the addresses id may not take: those of other
// instances and creates in progress, and those in their reuse hold. The caller holds
// macOSAddressMu.
func (m *manager) unavailableMacOSAddresses(id string) (map[netip.Addr]bool, error) {
	unavailable := make(map[netip.Addr]bool)
	err := m.forEachOtherInstance(id, "list instance addresses", func(meta *metadata) error {
		if addr, err := netip.ParseAddr(meta.IP); err == nil {
			unavailable[addr] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for other, addr := range m.macOSPendingAddresses {
		if other != id {
			unavailable[addr] = true
		}
	}
	held, err := m.loadMacOSAddressReleases()
	if err != nil {
		return nil, err
	}
	now := m.nowUTC()
	for ip, at := range held {
		if addr, err := netip.ParseAddr(ip); err == nil && now.Sub(at) < macOSAddressReuseHold {
			unavailable[addr] = true
		}
	}
	return unavailable, nil
}

// releaseMacOSAddress starts the reuse hold for a guest's address. It runs before the
// instance's data is removed, and an error stops the delete, so an address is never
// free without its hold.
func (m *manager) releaseMacOSAddress(id, ip string) error {
	m.macOSAddressMu.Lock()
	defer m.macOSAddressMu.Unlock()
	delete(m.macOSPendingAddresses, id)
	if ip == "" {
		return nil
	}
	held, err := m.loadMacOSAddressReleases()
	if err != nil {
		return err
	}
	now := m.nowUTC()
	for addr, at := range held {
		if now.Sub(at) >= macOSAddressReuseHold {
			delete(held, addr)
		}
	}
	held[ip] = now
	data, err := json.Marshal(held)
	if err != nil {
		return err
	}
	path := filepath.Join(m.paths.DataDir(), macOSAddressReleasesFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("record released macOS guest address: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("record released macOS guest address: %w", err)
	}
	return nil
}

// forgetMacOSAddress drops an address assigned to a create that did not finish.
func (m *manager) forgetMacOSAddress(id string) {
	m.macOSAddressMu.Lock()
	defer m.macOSAddressMu.Unlock()
	delete(m.macOSPendingAddresses, id)
}

func (m *manager) loadMacOSAddressReleases() (map[string]time.Time, error) {
	held := make(map[string]time.Time)
	data, err := os.ReadFile(filepath.Join(m.paths.DataDir(), macOSAddressReleasesFile))
	if errors.Is(err, os.ErrNotExist) {
		return held, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read released macOS guest addresses: %w", err)
	}
	if err := json.Unmarshal(data, &held); err != nil {
		return nil, fmt.Errorf("decode released macOS guest addresses: %w", err)
	}
	return held, nil
}
