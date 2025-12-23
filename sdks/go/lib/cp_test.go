package lib

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wsMessage represents a message to be sent/received
type wsMessage struct {
	Type int
	Data []byte
}

// MockWsConn implements WsConn for testing
type MockWsConn struct {
	writtenMessages []wsMessage
	readQueue       []wsMessage
	readIndex       int
	closed          bool
}

func (m *MockWsConn) WriteMessage(messageType int, data []byte) error {
	m.writtenMessages = append(m.writtenMessages, wsMessage{Type: messageType, Data: data})
	return nil
}

func (m *MockWsConn) ReadMessage() (int, []byte, error) {
	if m.readIndex >= len(m.readQueue) {
		return 0, nil, &websocket.CloseError{Code: websocket.CloseNormalClosure}
	}
	msg := m.readQueue[m.readIndex]
	m.readIndex++
	return msg.Type, msg.Data, nil
}

func (m *MockWsConn) Close() error {
	m.closed = true
	return nil
}

// MockWsDialer implements WsDialer for testing
type MockWsDialer struct {
	conn     *MockWsConn
	dialErr  error
	dialResp *http.Response
}

func (d *MockWsDialer) DialContext(ctx context.Context, url string, headers http.Header) (WsConn, *http.Response, error) {
	if d.dialErr != nil {
		return nil, d.dialResp, d.dialErr
	}
	return d.conn, nil, nil
}

// TestSanitizePath tests the path sanitization function
func TestSanitizePath(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		path    string
		want    string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "normal file",
			base:    "/dest",
			path:    "file.txt",
			want:    "/dest/file.txt",
			wantErr: false,
		},
		{
			name:    "subdirectory file",
			base:    "/dest",
			path:    "sub/dir/file.txt",
			want:    "/dest/sub/dir/file.txt",
			wantErr: false,
		},
		{
			name:    "path traversal attack",
			base:    "/dest",
			path:    "../../../etc/passwd",
			wantErr: true,
			errMsg:  "path escapes destination",
		},
		{
			name:    "absolute path attack",
			base:    "/dest",
			path:    "/etc/passwd",
			wantErr: true,
			errMsg:  "absolute paths not allowed",
		},
		{
			name:    "dot-dot in middle",
			base:    "/dest",
			path:    "sub/../../../etc/passwd",
			wantErr: true,
			errMsg:  "path escapes destination",
		},
		{
			name:    "current dir reference",
			base:    "/dest",
			path:    "./file.txt",
			want:    "/dest/file.txt",
			wantErr: false,
		},
		{
			name:    "empty path",
			base:    "/dest",
			path:    "",
			want:    "/dest",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sanitizePath(tt.base, tt.path)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// TestCpToInstance_SingleFile tests copying a single file
func TestCpToInstance_SingleFile(t *testing.T) {
	// Create a temp file to copy
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "test.txt")
	err := os.WriteFile(srcFile, []byte("hello world"), 0644)
	require.NoError(t, err)

	// Create mock that returns success
	successResult, _ := json.Marshal(cpResult{Type: "result", Success: true, BytesWritten: 11})
	mockConn := &MockWsConn{
		readQueue: []wsMessage{
			{Type: websocket.TextMessage, Data: successResult},
		},
	}

	mockDialer := &MockWsDialer{conn: mockConn}

	err = CpToInstance(context.Background(), CpConfig{
		BaseURL: "http://localhost:8080",
		APIKey:  "test-key",
	}, CpToInstanceOptions{
		InstanceID: "inst_123",
		SrcPath:    srcFile,
		DstPath:    "/app/test.txt",
		Dialer:     mockDialer,
	})

	require.NoError(t, err)
	assert.True(t, mockConn.closed)

	// Verify messages were sent
	require.GreaterOrEqual(t, len(mockConn.writtenMessages), 2) // request + data + end

	// First message should be JSON request
	var req cpRequest
	err = json.Unmarshal(mockConn.writtenMessages[0].Data, &req)
	require.NoError(t, err)
	assert.Equal(t, "to", req.Direction)
	assert.Equal(t, "/app/test.txt", req.GuestPath)
	assert.False(t, req.IsDir)
}

// TestCpFromInstance_PathTraversal tests that path traversal is rejected
func TestCpFromInstance_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock that sends a malicious header
	maliciousHeader, _ := json.Marshal(cpFileHeader{
		Type:   "header",
		Path:   "../../../etc/passwd",
		Mode:   0644,
		IsDir:  false,
		Size:   100,
	})

	mockConn := &MockWsConn{
		readQueue: []wsMessage{
			{Type: websocket.TextMessage, Data: maliciousHeader},
		},
	}

	mockDialer := &MockWsDialer{conn: mockConn}

	err := CpFromInstance(context.Background(), CpConfig{
		BaseURL: "http://localhost:8080",
		APIKey:  "test-key",
	}, CpFromInstanceOptions{
		InstanceID: "inst_123",
		SrcPath:    "/app/file.txt",
		DstPath:    tmpDir,
		Dialer:     mockDialer,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid path from server")
}

// TestCpFromInstance_AbsoluteSymlinkTarget tests that absolute symlink targets are rejected
func TestCpFromInstance_AbsoluteSymlinkTarget(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock that sends a symlink with absolute target
	maliciousHeader, _ := json.Marshal(cpFileHeader{
		Type:       "header",
		Path:       "link",
		Mode:       0777,
		IsSymlink:  true,
		LinkTarget: "/etc/passwd",
	})

	mockConn := &MockWsConn{
		readQueue: []wsMessage{
			{Type: websocket.TextMessage, Data: maliciousHeader},
		},
	}

	mockDialer := &MockWsDialer{conn: mockConn}

	err := CpFromInstance(context.Background(), CpConfig{
		BaseURL: "http://localhost:8080",
		APIKey:  "test-key",
	}, CpFromInstanceOptions{
		InstanceID: "inst_123",
		SrcPath:    "/app/",
		DstPath:    tmpDir,
		Dialer:     mockDialer,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid symlink target")
}

// TestCpFromInstance_TraversingSymlinkTarget tests that traversing symlink targets are rejected
func TestCpFromInstance_TraversingSymlinkTarget(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock that sends a symlink with traversing target
	maliciousHeader, _ := json.Marshal(cpFileHeader{
		Type:       "header",
		Path:       "link",
		Mode:       0777,
		IsSymlink:  true,
		LinkTarget: "../../../etc/passwd",
	})

	mockConn := &MockWsConn{
		readQueue: []wsMessage{
			{Type: websocket.TextMessage, Data: maliciousHeader},
		},
	}

	mockDialer := &MockWsDialer{conn: mockConn}

	err := CpFromInstance(context.Background(), CpConfig{
		BaseURL: "http://localhost:8080",
		APIKey:  "test-key",
	}, CpFromInstanceOptions{
		InstanceID: "inst_123",
		SrcPath:    "/app/",
		DstPath:    tmpDir,
		Dialer:     mockDialer,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid symlink target")
}

// TestCpFromInstance_NormalFile tests successful file copy from instance
func TestCpFromInstance_NormalFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock that sends a valid file
	header, _ := json.Marshal(cpFileHeader{
		Type:  "header",
		Path:  "output.txt",
		Mode:  0644,
		IsDir: false,
		Size:  11,
		Mtime: 1234567890,
	})
	endMarker, _ := json.Marshal(cpEndMarker{Type: "end", Final: true})

	mockConn := &MockWsConn{
		readQueue: []wsMessage{
			{Type: websocket.TextMessage, Data: header},
			{Type: websocket.BinaryMessage, Data: []byte("hello world")},
			{Type: websocket.TextMessage, Data: endMarker},
		},
	}

	mockDialer := &MockWsDialer{conn: mockConn}

	err := CpFromInstance(context.Background(), CpConfig{
		BaseURL: "http://localhost:8080",
		APIKey:  "test-key",
	}, CpFromInstanceOptions{
		InstanceID: "inst_123",
		SrcPath:    "/app/output.txt",
		DstPath:    tmpDir,
		Dialer:     mockDialer,
	})

	require.NoError(t, err)

	// Verify file was created
	content, err := os.ReadFile(filepath.Join(tmpDir, "output.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(content))
}

// TestCpCallbacks tests that callbacks are invoked correctly
func TestCpCallbacks(t *testing.T) {
	// Create a temp file to copy
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "test.txt")
	err := os.WriteFile(srcFile, []byte("hello world"), 0644)
	require.NoError(t, err)

	// Track callback invocations
	var fileStartCalled bool
	var progressCalled bool
	var fileEndCalled bool
	var progressBytes int64

	callbacks := &CpCallbacks{
		OnFileStart: func(path string, size int64) {
			fileStartCalled = true
			assert.Equal(t, srcFile, path)
			assert.Equal(t, int64(11), size)
		},
		OnProgress: func(bytesCopied int64) {
			progressCalled = true
			progressBytes = bytesCopied
		},
		OnFileEnd: func(path string) {
			fileEndCalled = true
		},
	}

	// Create mock that returns success
	successResult, _ := json.Marshal(cpResult{Type: "result", Success: true, BytesWritten: 11})
	mockConn := &MockWsConn{
		readQueue: []wsMessage{
			{Type: websocket.TextMessage, Data: successResult},
		},
	}

	mockDialer := &MockWsDialer{conn: mockConn}

	err = CpToInstance(context.Background(), CpConfig{
		BaseURL: "http://localhost:8080",
		APIKey:  "test-key",
	}, CpToInstanceOptions{
		InstanceID: "inst_123",
		SrcPath:    srcFile,
		DstPath:    "/app/test.txt",
		Callbacks:  callbacks,
		Dialer:     mockDialer,
	})

	require.NoError(t, err)
	assert.True(t, fileStartCalled, "OnFileStart should be called")
	assert.True(t, progressCalled, "OnProgress should be called")
	assert.True(t, fileEndCalled, "OnFileEnd should be called")
	assert.Equal(t, int64(11), progressBytes)
}

// TestBuildWsURL tests WebSocket URL construction
func TestBuildWsURL(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		instanceID string
		want       string
		wantErr    bool
	}{
		{
			name:       "https to wss",
			baseURL:    "https://api.example.com",
			instanceID: "inst_123",
			want:       "wss://api.example.com/instances/inst_123/cp",
		},
		{
			name:       "http to ws",
			baseURL:    "http://localhost:8080",
			instanceID: "inst_456",
			want:       "ws://localhost:8080/instances/inst_456/cp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildWsURL(tt.baseURL, tt.instanceID)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

