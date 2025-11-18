package system

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

const (
	StreamStdin  byte = 0
	StreamStdout byte = 1
	StreamStderr byte = 2
	StreamError  byte = 3
	StreamResize byte = 4
)

type ExecOptions struct {
	Command    []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	TTY        bool
	ResizeChan <-chan TerminalSize
}

type TerminalSize struct {
	Width  uint16
	Height uint16
}

type ExitStatus struct {
	Code int
}

// ExecIntoInstance executes command in instance via vsock
// vsockSocketPath is the Unix socket created by Cloud Hypervisor (e.g., /var/lib/hypeman/guests/{id}/vsock.sock)
func ExecIntoInstance(ctx context.Context, vsockSocketPath string, opts ExecOptions) (*ExitStatus, error) {
	// Connect to Cloud Hypervisor's vsock Unix socket
	conn, err := net.Dial("unix", vsockSocketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to vsock socket: %w", err)
	}
	defer conn.Close()

	// Send the port number per Cloud Hypervisor protocol
	if _, err := fmt.Fprintf(conn, "CONNECT 2222\n"); err != nil {
		return nil, fmt.Errorf("send vsock port: %w", err)
	}

	// Read handshake response (OK <cid>)
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read handshake response: %w", err)
	}

	if !strings.HasPrefix(response, "OK ") {
		return nil, fmt.Errorf("handshake failed: %s", strings.TrimSpace(response))
	}

	// Send exec request as first stdin frame
	req := struct {
		Command []string `json:"command"`
		TTY     bool     `json:"tty"`
	}{
		Command: opts.Command,
		TTY:     opts.TTY,
	}
	reqData, _ := json.Marshal(req)
	if err := sendFrame(conn, StreamStdin, reqData); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	var wg sync.WaitGroup
	exitChan := make(chan *ExitStatus, 1)
	errChan := make(chan error, 3)

	// stdin -> guest
	if opts.Stdin != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 32*1024)
			for {
				n, err := opts.Stdin.Read(buf)
				if n > 0 {
					if err := sendFrame(conn, StreamStdin, buf[:n]); err != nil {
						errChan <- err
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}

	// Handle terminal resize
	if opts.ResizeChan != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case size, ok := <-opts.ResizeChan:
					if !ok {
						return
					}
					resize := struct {
						Width  uint16 `json:"width"`
						Height uint16 `json:"height"`
					}{Width: size.Width, Height: size.Height}
					data, _ := json.Marshal(resize)
					sendFrame(conn, StreamResize, data)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// guest -> stdout/stderr/exit
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			streamType, data, err := readFrame(conn)
			if err != nil {
				if err == io.EOF {
					// If we get EOF without having received an exit code (which would return early),
					// it's an error
					errChan <- fmt.Errorf("unexpected EOF (no exit code received)")
				} else {
					errChan <- err
				}
				return
			}

			switch streamType {
			case StreamStdout:
				if opts.Stdout != nil {
					opts.Stdout.Write(data)
				}
			case StreamStderr:
				if opts.Stderr != nil {
					opts.Stderr.Write(data)
				}
			case StreamError:
				// Try to parse as exit status
				var exit struct {
					Status struct {
						Code int `json:"code"`
					} `json:"status"`
				}
				if json.Unmarshal(data, &exit) == nil {
					exitChan <- &ExitStatus{Code: exit.Status.Code}
					return
				}
				// Otherwise it's an error message
				errChan <- fmt.Errorf("guest error: %s", string(data))
				return
			}
		}
	}()

	// Wait for completion
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errChan:
		return nil, err
	case exit := <-exitChan:
		return exit, nil
	case <-done:
		return &ExitStatus{Code: 0}, nil
	}
}

func readFrame(conn net.Conn) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}

	streamType := header[0]
	length := binary.BigEndian.Uint32(header[1:5])
	
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return 0, nil, err
	}

	return streamType, data, nil
}

func sendFrame(conn net.Conn, streamType byte, data []byte) error {
	header := make([]byte, 5)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[1:5], uint32(len(data)))

	if _, err := conn.Write(header); err != nil {
		return err
	}
	if _, err := conn.Write(data); err != nil {
		return err
	}
	return nil
}

