//go:build darwin

package instances

import (
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func cloneMacOSStorage(root, disk, aux string) error {
	for src, dst := range map[string]string{root: disk, filepath.Join(filepath.Dir(root), "aux.img"): aux} {
		if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY); err != nil {
			return err
		}
		if err := os.Chmod(dst, 0600); err != nil {
			return err
		}
	}
	return nil
}

// vmnet stores octets without leading zeroes. A lease is observed addressing,
// not a static-IP assignment or a guest-readiness signal.
func macOSGuestIP(mac string) string {
	wanted, err := net.ParseMAC(mac)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return ""
	}
	return macOSLeaseIP(string(data), wanted)
}
func macOSLeaseIP(data string, wanted net.HardwareAddr) string {
	for _, block := range strings.Split(data, "}") {
		var addr, ip string
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			switch k {
			case "hw_address":
				_, addr, _ = strings.Cut(v, ",")
			case "ip_address":
				ip = v
			}
		}
		var padded []string
		for _, octet := range strings.Split(addr, ":") {
			if len(octet) == 1 {
				octet = "0" + octet
			}
			padded = append(padded, octet)
		}
		raw, err := hex.DecodeString(strings.Join(padded, ""))
		if err == nil && string(raw) == string(wanted) && net.ParseIP(ip) != nil {
			return ip
		}
	}
	return ""
}
