//go:build darwin

package vz

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/Code-Hex/vz/v3"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// VsockDialer implements hypervisor.VsockDialer for vz.
type VsockDialer struct {
	socketPath string // used as connection pool key
	cid        int64  // unused by vz but kept for interface compatibility
	vm         *vz.VirtualMachine
	mu         sync.RWMutex
}

// NewVsockDialer creates a new VsockDialer for vz.
func NewVsockDialer(vsockSocket string, vsockCID int64) hypervisor.VsockDialer {
	return &VsockDialer{
		socketPath: vsockSocket,
		cid:        vsockCID,
	}
}

// Key returns a unique identifier for this dialer, used for connection pooling.
func (d *VsockDialer) Key() string {
	return fmt.Sprintf("vz:%s", d.socketPath)
}

// SetVM sets the VirtualMachine for this dialer.
// This must be called after the VM starts, before DialVsock.
func (d *VsockDialer) SetVM(vm *vz.VirtualMachine) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.vm = vm
}

// DialVsock connects to the guest on the specified port.
func (d *VsockDialer) DialVsock(ctx context.Context, port int) (net.Conn, error) {
	d.mu.RLock()
	vm := d.vm
	d.mu.RUnlock()

	if vm == nil {
		return nil, fmt.Errorf("VM not set on VsockDialer - call SetVM first")
	}

	socketDevices := vm.SocketDevices()
	if len(socketDevices) == 0 {
		return nil, fmt.Errorf("no vsock device configured on VM")
	}

	conn, err := socketDevices[0].Connect(uint32(port))
	if err != nil {
		return nil, fmt.Errorf("vsock connect to port %d: %w", port, err)
	}

	return conn, nil
}

// VsockDialerWithVM creates a VsockDialer that's pre-configured with a VM.
// This is a convenience function for when you have the VM already.
func VsockDialerWithVM(vm *vz.VirtualMachine, socketPath string) *VsockDialer {
	return &VsockDialer{
		socketPath: socketPath,
		vm:         vm,
	}
}
