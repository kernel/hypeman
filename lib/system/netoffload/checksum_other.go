//go:build !linux

package netoffload

func DisableTXChecksum(string) error {
	return nil
}
