//go:build windows

package hypervisor

func SocketCacheKey(socketPath string) string { return socketPath }
