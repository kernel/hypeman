//go:build linux

package netoffload

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ethSSFeatures = 4
	ethGStringLen = 32
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

func DisableTXChecksum(iface string) error {
	if err := setEthtoolFeature(iface, "tx-checksum-ip-generic", false); err == nil {
		return nil
	}
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

func setEthtoolFeature(iface, name string, enabled bool) error {
	features, err := ethtoolFeatureNames(iface)
	if err != nil {
		return err
	}

	index := -1
	for i, feature := range features {
		if feature == name {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("feature %q not found", name)
	}

	blocks := (len(features) + 31) / 32
	buf := make([]byte, 8+blocks*8)
	binary.LittleEndian.PutUint32(buf[0:4], unix.ETHTOOL_SFEATURES)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(blocks))

	blockOffset := 8 + (index/32)*8
	bit := uint32(1) << uint(index%32)
	binary.LittleEndian.PutUint32(buf[blockOffset:blockOffset+4], bit)
	if enabled {
		binary.LittleEndian.PutUint32(buf[blockOffset+4:blockOffset+8], bit)
	}

	return ethtoolDo(iface, buf)
}

func ethtoolFeatureNames(iface string) ([]string, error) {
	count, err := ethtoolFeatureCount(iface)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, fmt.Errorf("no ethtool features reported")
	}

	buf := make([]byte, 12+count*ethGStringLen)
	binary.LittleEndian.PutUint32(buf[0:4], unix.ETHTOOL_GSTRINGS)
	binary.LittleEndian.PutUint32(buf[4:8], ethSSFeatures)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(count))

	if err := ethtoolDo(iface, buf); err != nil {
		return nil, err
	}

	names := make([]string, 0, count)
	for i := 0; i < count; i++ {
		raw := buf[12+i*ethGStringLen : 12+(i+1)*ethGStringLen]
		name := strings.TrimRight(string(raw), "\x00")
		names = append(names, name)
	}
	return names, nil
}

func ethtoolFeatureCount(iface string) (int, error) {
	buf := make([]byte, 20)
	binary.LittleEndian.PutUint32(buf[0:4], unix.ETHTOOL_GSSET_INFO)
	binary.LittleEndian.PutUint64(buf[8:16], 1<<ethSSFeatures)

	if err := ethtoolDo(iface, buf); err != nil {
		return 0, err
	}
	return int(binary.LittleEndian.Uint32(buf[16:20])), nil
}

func ethtoolDo(iface string, data []byte) error {
	if len(iface) >= unix.IFNAMSIZ {
		return unix.EINVAL
	}
	if len(data) == 0 {
		return unix.EINVAL
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	var ifr ifreqData
	copy(ifr.Name[:], iface)
	ifr.Data = unsafe.Pointer(&data[0])

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.SIOCETHTOOL), uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return fmt.Errorf("SIOCETHTOOL %s: %w", iface, errno)
	}
	return nil
}
