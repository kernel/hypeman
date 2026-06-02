//go:build !linux

package instances

func hostNetworkDiagnostics(tapDevice string) string {
	return ""
}
