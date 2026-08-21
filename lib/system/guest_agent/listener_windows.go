//go:build windows

package main

import (
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	ioctlGetViosockAF = 0x0801300c
	vmAddrCIDAny      = ^uint32(0)
	sockStream        = 1
	socketError       = ^uintptr(0)
)

type rawSockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Zero      [4]byte
}

type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

var (
	winsockOnce sync.Once
	winsockErr  error
	ws2DLL      = windows.NewLazySystemDLL("ws2_32.dll")
	bindProc    = ws2DLL.NewProc("bind")
	acceptProc  = ws2DLL.NewProc("accept")
)

func initializeWinsock() error {
	winsockOnce.Do(func() {
		var data windows.WSAData
		winsockErr = windows.WSAStartup(0x202, &data)
	})
	return winsockErr
}

func viosockAddressFamily() (int32, error) {
	devicePath, err := windows.UTF16PtrFromString(`\\.\Viosock`)
	if err != nil {
		return 0, err
	}
	device, err := windows.CreateFile(devicePath, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("open Viosock device: %w", err)
	}
	defer windows.CloseHandle(device)

	var family uint32
	var returned uint32
	if err := windows.DeviceIoControl(
		device,
		ioctlGetViosockAF,
		nil,
		0,
		(*byte)(unsafe.Pointer(&family)),
		uint32(unsafe.Sizeof(family)),
		&returned,
		nil,
	); err != nil {
		return 0, fmt.Errorf("query Viosock address family: %w", err)
	}
	if returned != uint32(unsafe.Sizeof(family)) || family == 0 {
		return 0, fmt.Errorf("Viosock returned invalid address family %d", family)
	}
	return int32(family), nil
}

func listenVSock(port uint32) (net.Listener, error) {
	if err := initializeWinsock(); err != nil {
		return nil, fmt.Errorf("initialize Winsock: %w", err)
	}
	family, err := viosockAddressFamily()
	if err != nil {
		return nil, err
	}

	handle, err := windows.WSASocket(family, sockStream, 0, nil, 0, windows.WSA_FLAG_OVERLAPPED)
	if err != nil {
		return nil, fmt.Errorf("create vsock: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = windows.Closesocket(handle)
		}
	}()

	addr := rawSockaddrVM{Family: uint16(family), Port: port, CID: vmAddrCIDAny}
	result, _, callErr := bindProc.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if result == socketError {
		return nil, fmt.Errorf("bind vsock port %d: %w", port, callErr)
	}
	if err := windows.Listen(handle, 128); err != nil {
		return nil, fmt.Errorf("listen on vsock port %d: %w", port, err)
	}

	closeOnError = false
	return &windowsVSockListener{handle: handle, addr: vsockAddr{cid: vmAddrCIDAny, port: port}}, nil
}

type windowsVSockListener struct {
	handle windows.Handle
	addr   vsockAddr
	once   sync.Once
}

func (l *windowsVSockListener) Accept() (net.Conn, error) {
	var peer rawSockaddrVM
	peerLen := int32(unsafe.Sizeof(peer))
	result, _, callErr := acceptProc.Call(
		uintptr(l.handle),
		uintptr(unsafe.Pointer(&peer)),
		uintptr(unsafe.Pointer(&peerLen)),
	)
	if result == socketError {
		return nil, fmt.Errorf("accept vsock connection: %w", callErr)
	}
	handle := windows.Handle(result)
	return &windowsVSockConn{handle: handle, local: l.addr, remote: vsockAddr{cid: peer.CID, port: peer.Port}}, nil
}

func (l *windowsVSockListener) Close() error {
	var err error
	l.once.Do(func() { err = windows.Closesocket(l.handle) })
	return err
}

func (l *windowsVSockListener) Addr() net.Addr { return l.addr }

type windowsVSockConn struct {
	handle windows.Handle
	local  net.Addr
	remote net.Addr
	once   sync.Once
}

func (c *windowsVSockConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := windows.WSABuf{Len: uint32(len(p)), Buf: &p[0]}
	var received, flags uint32
	err := windows.WSARecv(c.handle, &buf, 1, &received, &flags, nil, nil)
	runtime.KeepAlive(p)
	if err != nil {
		return int(received), err
	}
	if received == 0 {
		return 0, io.EOF
	}
	return int(received), nil
}

func (c *windowsVSockConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := windows.WSABuf{Len: uint32(len(p)), Buf: &p[0]}
	var sent uint32
	err := windows.WSASend(c.handle, &buf, 1, &sent, 0, nil, nil)
	runtime.KeepAlive(p)
	return int(sent), err
}

func (c *windowsVSockConn) Close() error {
	var err error
	c.once.Do(func() { err = windows.Closesocket(c.handle) })
	return err
}
func (c *windowsVSockConn) LocalAddr() net.Addr              { return c.local }
func (c *windowsVSockConn) RemoteAddr() net.Addr             { return c.remote }
func (c *windowsVSockConn) SetDeadline(time.Time) error      { return nil }
func (c *windowsVSockConn) SetReadDeadline(time.Time) error  { return nil }
func (c *windowsVSockConn) SetWriteDeadline(time.Time) error { return nil }

func defaultReadyFilePath() string {
	return `C:\ProgramData\Hypeman\guest-agent-ready`
}

var _ net.Listener = (*windowsVSockListener)(nil)
var _ net.Conn = (*windowsVSockConn)(nil)
