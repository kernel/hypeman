//go:build linux

package netoffload

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

type ethtoolValue struct {
	Cmd  uint32
	Data uint32
}

type ifreqData struct {
	Name [unix.IFNAMSIZ]byte
	Data unsafe.Pointer
	_    [24 - unsafe.Sizeof(uintptr(0))]byte
}

// DisableTXChecksum disables guest-side transmit checksum offload. Some VMM/TAP
// combinations deliver guest-to-host TCP with partial checksums otherwise.
func DisableTXChecksum(iface string) error {
	return setEthtoolValue(iface, unix.ETHTOOL_STXCSUM, 0)
}

func setEthtoolValue(iface string, cmd uint32, data uint32) error {
	if len(iface) >= unix.IFNAMSIZ {
		return unix.EINVAL
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	value := ethtoolValue{Cmd: cmd, Data: data}
	var ifr ifreqData
	copy(ifr.Name[:], iface)
	ifr.Data = unsafe.Pointer(&value)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.SIOCETHTOOL), uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return fmt.Errorf("SIOCETHTOOL %s: %w", iface, errno)
	}
	return nil
}
