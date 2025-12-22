// Package lib provides manually-maintained functionality that extends the auto-generated SDK.
package lib

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/onkernel/hypeman-go/internal/requestconfig"
)

// CpConfig holds the configuration needed for copy operations.
// Extract this from a hypeman.Client using ExtractCpConfig.
type CpConfig struct {
	// BaseURL is the base URL for the hypeman API
	BaseURL string
	// APIKey is the JWT token for authentication
	APIKey string
}

// ExtractCpConfig extracts the base URL and API key from client options.
func ExtractCpConfig(opts []requestconfig.RequestOption) (CpConfig, error) {
	cfg := &requestconfig.RequestConfig{}
	if err := cfg.Apply(opts...); err != nil {
		return CpConfig{}, fmt.Errorf("apply options: %w", err)
	}

	baseURL := cfg.BaseURL
	if baseURL == nil {
		baseURL = cfg.DefaultBaseURL
	}
	if baseURL == nil {
		return CpConfig{}, fmt.Errorf("base URL not configured")
	}

	return CpConfig{
		BaseURL: baseURL.String(),
		APIKey:  cfg.APIKey,
	}, nil
}

// CpCallbacks provides optional progress callbacks for copy operations.
type CpCallbacks struct {
	OnFileStart func(path string, size int64) // Called when a file starts copying
	OnProgress  func(bytesCopied int64)       // Called as bytes are copied
	OnFileEnd   func(path string)             // Called when a file finishes copying
}

// CpToInstanceOptions configures a copy-to-instance operation
type CpToInstanceOptions struct {
	InstanceID  string       // Instance ID to copy to
	SrcPath     string       // Local source path
	DstPath     string       // Destination path in guest
	Mode        fs.FileMode  // Optional: override file mode (0 = auto-detect)
	Archive     bool         // Preserve UID/GID ownership
	FollowLinks bool         // Follow symbolic links when copying
	Callbacks   *CpCallbacks // Optional: progress callbacks
	Dialer      WsDialer     // Optional: custom WebSocket dialer (for testing)
}

// CpFromInstanceOptions configures a copy-from-instance operation
type CpFromInstanceOptions struct {
	InstanceID  string       // Instance ID to copy from
	SrcPath     string       // Source path in guest
	DstPath     string       // Local destination path
	FollowLinks bool         // Follow symbolic links
	Archive     bool         // Preserve UID/GID ownership
	Callbacks   *CpCallbacks // Optional: progress callbacks
	Dialer      WsDialer     // Optional: custom WebSocket dialer (for testing)
}

// cpRequest is the JSON request sent over WebSocket
type cpRequest struct {
	Direction   string `json:"direction"`
	GuestPath   string `json:"guest_path"`
	IsDir       bool   `json:"is_dir,omitempty"`
	Mode        uint32 `json:"mode,omitempty"`
	FollowLinks bool   `json:"follow_links,omitempty"`
	Uid         uint32 `json:"uid,omitempty"`
	Gid         uint32 `json:"gid,omitempty"`
}

// cpFileHeader is received from the server when copying from guest
type cpFileHeader struct {
	Type       string `json:"type"`
	Path       string `json:"path"`
	Mode       uint32 `json:"mode"`
	IsDir      bool   `json:"is_dir"`
	IsSymlink  bool   `json:"is_symlink"`
	LinkTarget string `json:"link_target"`
	Size       int64  `json:"size"`
	Mtime      int64  `json:"mtime"`
	Uid        uint32 `json:"uid,omitempty"`
	Gid        uint32 `json:"gid,omitempty"`
}

// cpEndMarker signals end of file or transfer
type cpEndMarker struct {
	Type  string `json:"type"`
	Final bool   `json:"final"`
}

// cpResult is the response from a copy-to operation
type cpResult struct {
	Type         string `json:"type"`
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
	BytesWritten int64  `json:"bytes_written,omitempty"`
}

// cpError is an error message from the server
type cpError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

// CpToInstance copies a file or directory to a running instance.
//
// Example:
//
//	cfg, _ := lib.ExtractCpConfig(client.Options)
//	err := lib.CpToInstance(ctx, cfg, lib.CpToInstanceOptions{
//	    InstanceID: "inst_123",
//	    SrcPath:    "./local-file.txt",
//	    DstPath:    "/app/file.txt",
//	})
func CpToInstance(ctx context.Context, cfg CpConfig, opts CpToInstanceOptions) error {
	return cpToInstanceInternal(ctx, cfg, opts, nil)
}

// cpToInstanceInternal is the internal implementation that accepts visitedDirs for cycle detection
func cpToInstanceInternal(ctx context.Context, cfg CpConfig, opts CpToInstanceOptions, visitedDirs map[string]bool) error {
	// Build WebSocket URL
	wsURL, err := buildWsURL(cfg.BaseURL, opts.InstanceID)
	if err != nil {
		return fmt.Errorf("build ws url: %w", err)
	}

	// Connect to WebSocket
	headers := http.Header{}
	headers.Set("Authorization", fmt.Sprintf("Bearer %s", cfg.APIKey))

	// Use provided dialer or default
	dialer := opts.Dialer
	if dialer == nil {
		dialer = &DefaultDialer{}
	}

	ws, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("websocket connect failed (HTTP %d): %s", resp.StatusCode, string(body))
		}
		return fmt.Errorf("websocket connect failed: %w", err)
	}
	defer ws.Close()

	// Stat the source
	srcInfo, err := os.Stat(opts.SrcPath)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	mode := opts.Mode
	if mode == 0 {
		mode = srcInfo.Mode().Perm()
	}

	// Get UID/GID if archive mode is enabled
	var uid, gid uint32
	if opts.Archive {
		if stat, ok := srcInfo.Sys().(*syscall.Stat_t); ok {
			uid = stat.Uid
			gid = stat.Gid
		}
	}

	// Send initial request
	req := cpRequest{
		Direction:   "to",
		GuestPath:   opts.DstPath,
		IsDir:       srcInfo.IsDir(),
		Mode:        uint32(mode),
		FollowLinks: opts.FollowLinks,
		Uid:         uid,
		Gid:         gid,
	}
	reqJSON, _ := json.Marshal(req)
	if err := ws.WriteMessage(websocket.TextMessage, reqJSON); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	if srcInfo.IsDir() {
		// Track visited directories to detect symlink cycles
		if visitedDirs == nil {
			visitedDirs = make(map[string]bool)
		}
		absPath, _ := filepath.Abs(opts.SrcPath)
		absPath, _ = filepath.EvalSymlinks(absPath)
		if !visitedDirs[absPath] {
			visitedDirs[absPath] = true
		}
		return copyDirToWs(ctx, cfg, ws, opts.SrcPath, opts.DstPath, opts.InstanceID, opts.Archive, opts.FollowLinks, opts.Dialer, opts.Callbacks, visitedDirs)
	}
	return copyFileToWs(ws, opts.SrcPath, srcInfo.Size(), opts.Callbacks)
}

// copyFileToWs copies a single file to the WebSocket
func copyFileToWs(ws WsConn, srcPath string, size int64, callbacks *CpCallbacks) error {
	file, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer file.Close()

	// Notify file start
	if callbacks != nil && callbacks.OnFileStart != nil {
		callbacks.OnFileStart(srcPath, size)
	}

	buf := make([]byte, 32*1024)
	var bytesSent int64
	for {
		n, err := file.Read(buf)
		if n > 0 {
			if sendErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); sendErr != nil {
				return fmt.Errorf("send data: %w", sendErr)
			}
			bytesSent += int64(n)
			// Notify progress
			if callbacks != nil && callbacks.OnProgress != nil {
				callbacks.OnProgress(bytesSent)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read source: %w", err)
		}
	}

	// Send end marker
	endMsg, _ := json.Marshal(map[string]string{"type": "end"})
	if err := ws.WriteMessage(websocket.TextMessage, endMsg); err != nil {
		return fmt.Errorf("send end: %w", err)
	}

	// Wait for result
	_, message, err := ws.ReadMessage()
	if err != nil {
		return fmt.Errorf("read result: %w", err)
	}

	// Check message type first - server may send error or result
	var msgType struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(message, &msgType); err != nil {
		return fmt.Errorf("parse message type: %w", err)
	}

	if msgType.Type == "error" {
		var errMsg cpError
		if err := json.Unmarshal(message, &errMsg); err != nil {
			return fmt.Errorf("parse error: %w", err)
		}
		return fmt.Errorf("copy failed: %s", errMsg.Message)
	}

	var result cpResult
	if err := json.Unmarshal(message, &result); err != nil {
		return fmt.Errorf("parse result: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("copy failed: %s", result.Error)
	}

	// Notify file end (srcPath not available here, use empty string)
	if callbacks != nil && callbacks.OnFileEnd != nil {
		callbacks.OnFileEnd(srcPath)
	}

	return nil
}

// copyDirToWs copies a directory to the WebSocket
func copyDirToWs(ctx context.Context, cfg CpConfig, ws WsConn, srcPath, dstPath, instanceID string, archive, followLinks bool, dialer WsDialer, callbacks *CpCallbacks, visitedDirs map[string]bool) error {
	// For directory copy, we just send the end marker
	// The server will create the directory
	endMsg, _ := json.Marshal(map[string]string{"type": "end"})
	if err := ws.WriteMessage(websocket.TextMessage, endMsg); err != nil {
		return fmt.Errorf("send end: %w", err)
	}

	// Wait for result
	_, message, err := ws.ReadMessage()
	if err != nil {
		return fmt.Errorf("read result: %w", err)
	}

	// Check message type first - server may send error or result
	var msgType struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(message, &msgType); err != nil {
		return fmt.Errorf("parse message type: %w", err)
	}

	if msgType.Type == "error" {
		var errMsg cpError
		if err := json.Unmarshal(message, &errMsg); err != nil {
			return fmt.Errorf("parse error: %w", err)
		}
		return fmt.Errorf("copy failed: %s", errMsg.Message)
	}

	var result cpResult
	if err := json.Unmarshal(message, &result); err != nil {
		return fmt.Errorf("parse result: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("copy failed: %s", result.Error)
	}

	// Now recursively copy contents
	return filepath.WalkDir(srcPath, func(walkPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if walkPath == srcPath {
			return nil // Skip root
		}

		relPath, err := filepath.Rel(srcPath, walkPath)
		if err != nil {
			return fmt.Errorf("relative path: %w", err)
		}

		// Use path.Join (not filepath.Join) for guest paths to ensure forward slashes
		// Convert Windows backslashes to forward slashes for Linux guest
		targetPath := path.Join(dstPath, filepath.ToSlash(relPath))
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("info: %w", err)
		}

		// Check for symlink cycles when following links
		if followLinks && info.Mode()&fs.ModeSymlink != 0 {
			// Resolve the symlink to its real path
			realPath, err := filepath.EvalSymlinks(walkPath)
			if err != nil {
				// If we can't resolve the symlink, skip it (might be broken)
				return nil
			}
			realInfo, err := os.Stat(realPath)
			if err != nil {
				return nil // Skip broken symlinks
			}
			// If it's a directory symlink, check for cycles
			if realInfo.IsDir() {
				if visitedDirs[realPath] {
					// Cycle detected, skip this symlink to prevent infinite recursion
					return nil
				}
				visitedDirs[realPath] = true
			}
		}

		// Determine the mode to use
		// For symlinks: if following links, let CpToInstance auto-detect from target
		// (symlinks show 0777 but that's not the target's actual mode)
		var mode fs.FileMode
		if info.Mode()&fs.ModeSymlink != 0 && followLinks {
			mode = 0 // Let CpToInstance use os.Stat to get target's mode
		} else {
			mode = info.Mode().Perm()
		}

		// For each file/dir, we need a new WebSocket connection
		// This is because the protocol is one-file-per-connection
		return cpToInstanceInternal(ctx, cfg, CpToInstanceOptions{
			InstanceID:  instanceID,
			SrcPath:     walkPath,
			DstPath:     targetPath,
			Mode:        mode,
			Archive:     archive,
			FollowLinks: followLinks,
			Dialer:      dialer,
			Callbacks:   callbacks,
		}, visitedDirs)
	})
}

// CpFromInstance copies a file or directory from a running instance.
//
// Example:
//
//	cfg, _ := lib.ExtractCpConfig(client.Options)
//	err := lib.CpFromInstance(ctx, cfg, lib.CpFromInstanceOptions{
//	    InstanceID: "inst_123",
//	    SrcPath:    "/app/output.txt",
//	    DstPath:    "./local-output.txt",
//	})
func CpFromInstance(ctx context.Context, cfg CpConfig, opts CpFromInstanceOptions) error {
	// Build WebSocket URL
	wsURL, err := buildWsURL(cfg.BaseURL, opts.InstanceID)
	if err != nil {
		return fmt.Errorf("build ws url: %w", err)
	}

	// Connect to WebSocket
	headers := http.Header{}
	headers.Set("Authorization", fmt.Sprintf("Bearer %s", cfg.APIKey))

	// Use provided dialer or default
	dialer := opts.Dialer
	if dialer == nil {
		dialer = &DefaultDialer{}
	}

	ws, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("websocket connect failed (HTTP %d): %s", resp.StatusCode, string(body))
		}
		return fmt.Errorf("websocket connect failed: %w", err)
	}
	defer ws.Close()

	// Send initial request
	req := cpRequest{
		Direction:   "from",
		GuestPath:   opts.SrcPath,
		FollowLinks: opts.FollowLinks,
	}
	reqJSON, _ := json.Marshal(req)
	if err := ws.WriteMessage(websocket.TextMessage, reqJSON); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	var currentFile *os.File
	var currentHeader *cpFileHeader
	var bytesReceived int64
	var receivedFinal bool

	// Ensure any open file is closed on function exit (fixes file handle leak)
	defer func() {
		if currentFile != nil {
			currentFile.Close()
		}
	}()

	for {
		msgType, message, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				break
			}
			return fmt.Errorf("read message: %w", err)
		}

		if msgType == websocket.TextMessage {
			// Parse JSON message
			var msgMap map[string]interface{}
			if err := json.Unmarshal(message, &msgMap); err != nil {
				return fmt.Errorf("parse message: %w", err)
			}

			msgType, _ := msgMap["type"].(string)

			switch msgType {
			case "header":
				// Close previous file if any
				if currentFile != nil {
					currentFile.Close()
					currentFile = nil
				}

				var header cpFileHeader
				if err := json.Unmarshal(message, &header); err != nil {
					return fmt.Errorf("parse header: %w", err)
				}
				currentHeader = &header

				// Sanitize server-provided path to prevent path traversal attacks
				targetPath, err := sanitizePath(opts.DstPath, header.Path)
				if err != nil {
					return fmt.Errorf("invalid path from server: %w", err)
				}

				if header.IsDir {
					if err := os.MkdirAll(targetPath, fs.FileMode(header.Mode)); err != nil {
						return fmt.Errorf("create directory %s: %w", targetPath, err)
					}
					// Apply ownership if archive mode
					if opts.Archive {
						os.Chown(targetPath, int(header.Uid), int(header.Gid))
					}
			} else if header.IsSymlink {
				// Validate symlink target to prevent pointing outside destination
				if filepath.IsAbs(header.LinkTarget) || strings.HasPrefix(filepath.Clean(header.LinkTarget), "..") {
					return fmt.Errorf("invalid symlink target: %s", header.LinkTarget)
				}
				// Create parent directory if needed
				if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
					return fmt.Errorf("create parent dir for symlink: %w", err)
				}
				os.Remove(targetPath)
				if err := os.Symlink(header.LinkTarget, targetPath); err != nil {
					return fmt.Errorf("create symlink %s: %w", targetPath, err)
				}
					// Apply ownership if archive mode (use Lchown for symlinks)
					if opts.Archive {
						os.Lchown(targetPath, int(header.Uid), int(header.Gid))
					}
				} else {
					if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
						return fmt.Errorf("create parent dir: %w", err)
					}
					f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(header.Mode))
					if err != nil {
						return fmt.Errorf("create file %s: %w", targetPath, err)
					}
					currentFile = f
					// Notify file start
					if opts.Callbacks != nil && opts.Callbacks.OnFileStart != nil {
						opts.Callbacks.OnFileStart(header.Path, header.Size)
					}
				}

			case "end":
				var endMarker cpEndMarker
				if err := json.Unmarshal(message, &endMarker); err != nil {
					return fmt.Errorf("invalid end marker: %w", err)
				}

				if currentFile != nil {
					currentFile.Close()
					if currentHeader != nil {
						// Path was already validated when file was created
						targetPath, _ := sanitizePath(opts.DstPath, currentHeader.Path)
						if currentHeader.Mtime > 0 {
							mtime := time.Unix(currentHeader.Mtime, 0)
							os.Chtimes(targetPath, mtime, mtime)
						}
						// Apply ownership if archive mode
						if opts.Archive {
							os.Chown(targetPath, int(currentHeader.Uid), int(currentHeader.Gid))
						}
						// Notify file end
						if opts.Callbacks != nil && opts.Callbacks.OnFileEnd != nil {
							opts.Callbacks.OnFileEnd(currentHeader.Path)
						}
					}
					currentFile = nil
					currentHeader = nil
					bytesReceived = 0 // Reset for next file
				}

				if endMarker.Final {
					receivedFinal = true
					return nil
				}

			case "error":
				var cpErr cpError
				json.Unmarshal(message, &cpErr)
				return fmt.Errorf("copy error at %s: %s", cpErr.Path, cpErr.Message)

			case "result":
				var result cpResult
				json.Unmarshal(message, &result)
				if !result.Success {
					return fmt.Errorf("copy failed: %s", result.Error)
				}
			}
		} else if msgType == websocket.BinaryMessage {
			// File data
			if currentFile != nil {
				n, err := currentFile.Write(message)
				if err != nil {
					return fmt.Errorf("write: %w", err)
				}
				bytesReceived += int64(n)
				// Notify progress
				if opts.Callbacks != nil && opts.Callbacks.OnProgress != nil {
					opts.Callbacks.OnProgress(bytesReceived)
				}
			}
		}
	}

	// If connection closed without receiving final marker, the transfer was incomplete
	if !receivedFinal {
		return fmt.Errorf("copy stream ended without completion marker")
	}
	return nil
}

// sanitizePath ensures the path doesn't escape the base directory.
// This prevents path traversal attacks where server-provided paths contain ".." components.
func sanitizePath(base, path string) (string, error) {
	// Clean the path to resolve any . or .. components
	cleaned := filepath.Clean(path)

	// Reject absolute paths
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("invalid path: absolute paths not allowed: %s", path)
	}

	// Reject paths that start with ..
	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("invalid path: path escapes destination: %s", path)
	}

	// Join with base and verify the result is under base
	result := filepath.Join(base, cleaned)
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve base path: %w", err)
	}
	absResult, err := filepath.Abs(result)
	if err != nil {
		return "", fmt.Errorf("resolve result path: %w", err)
	}

	// Ensure the result is under the base directory
	// Special case: if base is root ("/"), everything under it is valid
	isRoot := absBase == "/" || absBase == string(filepath.Separator)
	if !isRoot && !strings.HasPrefix(absResult, absBase+string(filepath.Separator)) && absResult != absBase {
		return "", fmt.Errorf("invalid path: path escapes destination: %s", path)
	}

	return result, nil
}

// buildWsURL builds the WebSocket URL for the cp endpoint
func buildWsURL(baseURL, instanceID string) (string, error) {
	// Validate instanceID to prevent path traversal attacks
	if instanceID == "" {
		return "", fmt.Errorf("instance ID cannot be empty")
	}
	if strings.Contains(instanceID, "/") || strings.Contains(instanceID, "\\") || strings.Contains(instanceID, "..") {
		return "", fmt.Errorf("invalid instance ID: contains path separator or traversal sequence")
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}

	// Append to existing path (preserves any path prefix like /api)
	// Use path.Join to handle trailing slashes and ensure clean paths
	u.Path = path.Join(u.Path, "instances", instanceID, "cp")

	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}

	return u.String(), nil
}

// CpToInstanceFromURL is a convenience function that uses base URL and API key directly.
func CpToInstanceFromURL(ctx context.Context, baseURL, apiKey string, opts CpToInstanceOptions) error {
	cfg := CpConfig{
		BaseURL: baseURL,
		APIKey:  apiKey,
	}
	return CpToInstance(ctx, cfg, opts)
}

// CpFromInstanceFromURL is a convenience function that uses base URL and API key directly.
func CpFromInstanceFromURL(ctx context.Context, baseURL, apiKey string, opts CpFromInstanceOptions) error {
	cfg := CpConfig{
		BaseURL: baseURL,
		APIKey:  apiKey,
	}
	return CpFromInstance(ctx, cfg, opts)
}

