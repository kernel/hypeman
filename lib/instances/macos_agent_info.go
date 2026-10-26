package instances

import (
	"context"
	"fmt"
	"slices"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
)

// maxGuestAgentProtocol is the newest agent protocol this host understands. A macOS
// agent ships inside its image, so it can be older or newer than the host.
const maxGuestAgentProtocol = 1

// GuestAgentInfo is what a macOS guest's agent reported when it became ready.
// Protocol 0 is an agent that predates GetAgentInfo.
type GuestAgentInfo struct {
	ProtocolVersion uint32
	Version         string   `json:",omitempty"`
	Capabilities    []string `json:",omitempty"`
}

// HasCapability reports whether the agent advertised an optional capability.
func (i *GuestAgentInfo) HasCapability(c guest.AgentCapability) bool {
	return i != nil && slices.Contains(i.Capabilities, c.String())
}

// macOSGuestAgentInfo asks a ready macOS agent what it supports. An agent speaking a
// newer protocol than this host is refused, so the guest stays Initializing with that
// reason instead of being driven with requests it may not honor.
func (m *manager) macOSGuestAgentInfo(ctx context.Context, stored *StoredMetadata) (*GuestAgentInfo, error) {
	fetch := m.guestAgentInfoProbe
	if fetch == nil {
		fetch = fetchGuestAgentInfo
	}
	resp, err := fetch(ctx, stored)
	if err != nil {
		return nil, fmt.Errorf("read guest agent info: %w", err)
	}
	if resp.ProtocolVersion > maxGuestAgentProtocol {
		return nil, fmt.Errorf("guest agent %q speaks protocol %d; this host supports up to %d", resp.Version, resp.ProtocolVersion, maxGuestAgentProtocol)
	}
	info := &GuestAgentInfo{ProtocolVersion: resp.ProtocolVersion, Version: resp.Version}
	for _, c := range resp.Capabilities {
		info.Capabilities = append(info.Capabilities, c.String())
	}
	return info, nil
}

func fetchGuestAgentInfo(ctx context.Context, stored *StoredMetadata) (*guest.GetAgentInfoResponse, error) {
	dialer, err := hypervisor.NewVsockDialer(stored.HypervisorType, stored.VsockSocket, stored.VsockCID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, guestAgentReadyProbeTimeout)
	defer cancel()
	return guest.GetAgentInfo(ctx, dialer)
}
