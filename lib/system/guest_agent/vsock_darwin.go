//go:build darwin && cgo

package main

/*
#include <sys/socket.h>
#include <sys/vsock.h>
#include <fcntl.h>
#include <errno.h>
#include <unistd.h>
static int listen_vm(unsigned port) {
 int fd=socket(AF_VSOCK,SOCK_STREAM,0);if(fd<0)return -1;
 struct sockaddr_vm a={0};a.svm_len=sizeof(a);a.svm_family=AF_VSOCK;a.svm_cid=VMADDR_CID_ANY;a.svm_port=port;
 if(bind(fd,(struct sockaddr*)&a,sizeof(a))<0 || listen(fd,16)<0 || fcntl(fd,F_SETFL,O_NONBLOCK)<0){int e=errno;close(fd);errno=e;return -1;}
 if(fcntl(fd,F_SETFD,FD_CLOEXEC)<0){int e=errno;close(fd);errno=e;return -1;}return fd;
}
static int accept_vm(int fd) {
 struct sockaddr_vm a={0};socklen_t n=sizeof(a);int c=accept(fd,(struct sockaddr*)&a,&n);if(c<0)return -1;
 if(a.svm_cid!=VMADDR_CID_HOST){close(c);errno=EAGAIN;return -1;}
 if(fcntl(c,F_SETFL,O_NONBLOCK)<0){int e=errno;close(c);errno=e;return -1;}
 if(fcntl(c,F_SETFD,FD_CLOEXEC)<0){int e=errno;close(c);errno=e;return -1;}return c;
}
*/
import "C"
import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

type vmAddr string

func (a vmAddr) Network() string { return "vsock" }
func (a vmAddr) String() string  { return string(a) }

// Do not promote os.File's zero-copy methods: AF_VSOCK is not a TCP/file
// descriptor and must use ordinary reads/writes for gRPC transport.
type vmConn struct {
	file  *os.File
	local net.Addr
}

func (c *vmConn) Read(p []byte) (int, error) {
	n, err := c.file.Read(p)
	if err != nil {
		return n, &net.OpError{Op: "read", Net: "vsock", Err: err}
	}
	return n, err
}
func (c *vmConn) Write(p []byte) (int, error) {
	n, err := c.file.Write(p)
	if err != nil {
		return n, &net.OpError{Op: "write", Net: "vsock", Err: err}
	}
	return n, err
}
func (c *vmConn) Close() error                       { return c.file.Close() }
func (c *vmConn) SetDeadline(t time.Time) error      { return c.file.SetDeadline(t) }
func (c *vmConn) SetReadDeadline(t time.Time) error  { return c.file.SetReadDeadline(t) }
func (c *vmConn) SetWriteDeadline(t time.Time) error { return c.file.SetWriteDeadline(t) }

func (c *vmConn) LocalAddr() net.Addr  { return c.local }
func (c *vmConn) RemoteAddr() net.Addr { return vmAddr("host:2") }

type vmListener struct {
	mu     sync.Mutex
	fd     C.int
	port   uint32
	closed bool
}

func listenVsock(port uint32) (net.Listener, error) {
	fd, err := C.listen_vm(C.uint(port))
	if fd < 0 {
		return nil, fmt.Errorf("listen vsock: %w", err)
	}
	return &vmListener{fd: fd, port: port}, nil
}
func (l *vmListener) Addr() net.Addr { return vmAddr(fmt.Sprintf("guest:%d", l.port)) }
func (l *vmListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return net.ErrClosed
	}
	l.closed = true
	C.close(l.fd)
	return nil
}
func (l *vmListener) Accept() (net.Conn, error) {
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, net.ErrClosed
		}
		// Darwin has no accept4: the accepted fd is inheritable until accept_vm sets FD_CLOEXEC.
		// Holding the fork lock across that window keeps a concurrent fork from copying it.
		syscall.ForkLock.RLock()
		fd, err := C.accept_vm(l.fd)
		syscall.ForkLock.RUnlock()
		l.mu.Unlock()
		if fd >= 0 {
			return &vmConn{file: os.NewFile(uintptr(fd), "guest-vsock"), local: l.Addr()}, nil
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return nil, fmt.Errorf("accept vsock: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
