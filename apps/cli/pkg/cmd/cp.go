package cmd

import (
	"archive/tar"
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
	"time"

	"github.com/gorilla/websocket"
	"github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/lib"
	"github.com/urfave/cli/v3"
)

// cpRequest represents the JSON body for cp requests
type cpRequest struct {
	Direction   string `json:"direction"`
	GuestPath   string `json:"guest_path"`
	IsDir       bool   `json:"is_dir,omitempty"`
	Mode        uint32 `json:"mode,omitempty"`
	FollowLinks bool   `json:"follow_links,omitempty"`
	Uid         uint32 `json:"uid"`
	Gid         uint32 `json:"gid"`
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

var cpCmd = cli.Command{
	Name:      "cp",
	Usage:     "Copy files/folders between an instance and the local filesystem",
	ArgsUsage: "<src> <dst>",
	Description: `Copy files between the local filesystem and an instance.

The path format is:
  - Local path: /path/to/file or ./relative/path
  - Instance path: <instance>:/path/in/instance

Examples:
  # Copy file to instance
  hypeman cp ./local-file.txt myinstance:/app/file.txt

  # Copy file from instance
  hypeman cp myinstance:/app/output.txt ./local-output.txt

  # Copy directory to instance
  hypeman cp ./local-dir myinstance:/app/dir

  # Copy directory from instance
  hypeman cp myinstance:/app/dir ./local-dir`,
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "archive",
			Aliases: []string{"a"},
			Usage:   "Archive mode (copy all uid/gid information)",
		},
		&cli.BoolFlag{
			Name:    "follow-links",
			Aliases: []string{"L"},
			Usage:   "Always follow symbolic links in source",
		},
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Suppress progress output during copy",
		},
	},
	Action:          handleCp,
	HideHelpCommand: true,
}

func handleCp(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) != 2 {
		return fmt.Errorf("exactly 2 arguments required: source and destination\nUsage: hypeman cp <src> <dst>")
	}

	srcArg := args[0]
	dstArg := args[1]

	// Parse source and destination
	srcInstance, srcPath, srcIsRemote := parseCpPath(srcArg)
	dstInstance, dstPath, dstIsRemote := parseCpPath(dstArg)

	// Validate: one must be local, one must be remote
	if srcIsRemote && dstIsRemote {
		return fmt.Errorf("cannot copy between two instances; one path must be local")
	}
	if !srcIsRemote && !dstIsRemote {
		return fmt.Errorf("at least one path must reference an instance (use instance:/path format)")
	}

	// Get client and resolve instance
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var instanceID string
	var err error
	if srcIsRemote {
		instanceID, err = ResolveInstance(ctx, &client, srcInstance)
	} else {
		instanceID, err = ResolveInstance(ctx, &client, dstInstance)
	}
	if err != nil {
		return err
	}

	// Get base URL and API key
	baseURL := cmd.Root().String("base-url")
	if baseURL == "" {
		baseURL = os.Getenv("HYPEMAN_BASE_URL")
	}
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	apiKey := os.Getenv("HYPEMAN_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("HYPEMAN_API_KEY environment variable required")
	}

	archive := cmd.Bool("archive")
	followLinks := cmd.Bool("follow-links")
	quiet := cmd.Bool("quiet")

	if srcIsRemote {
		// Copy from instance to local (or stdout if dstPath is "-")
		if dstPath == "-" {
			return copyFromInstanceToStdout(ctx, baseURL, apiKey, instanceID, srcPath, followLinks, archive)
		}
		return copyFromInstance(ctx, &client, baseURL, apiKey, instanceID, srcPath, dstPath, followLinks, quiet, archive)
	} else {
		// Copy from local (or stdin if srcPath is "-") to instance
		if srcPath == "-" {
			return copyFromStdinToInstance(ctx, baseURL, apiKey, instanceID, dstPath, archive)
		}
		return copyToInstance(ctx, &client, baseURL, apiKey, instanceID, srcPath, dstPath, quiet, archive, followLinks)
	}
}

// parseCpPath parses a path like "instance:/path" or "/local/path"
// Following docker cp conventions:
// - Paths starting with / or ./ or ../ or ~ are always local paths
// - If a path contains a colon, it's treated as instance:path UNLESS it's an explicit local path
// - For ambiguous cases (file:name.txt), use explicit paths like ./file:name.txt
func parseCpPath(path string) (instance, containerPath string, isRemote bool) {
	// Explicit local paths: absolute path, relative path with ./  or ../, or home directory
	if strings.HasPrefix(path, "/") ||
		strings.HasPrefix(path, "./") ||
		strings.HasPrefix(path, "../") ||
		strings.HasPrefix(path, "~") ||
		path == "." ||
		path == ".." {
		containerPath = path
		return
	}

	// Check for Windows drive path (e.g., C:\...)
	if isWindowsPath(path) {
		containerPath = path
		return
	}

	// Check for colon separator (instance:path format)
	colonIdx := strings.Index(path, ":")
	if colonIdx > 0 {
		potentialInstance := path[:colonIdx]

		// If the part before colon contains path separators, it's a local path with colon in name
		// This helps with edge cases like "some/path:with:colons"
		if strings.ContainsAny(potentialInstance, "/\\") {
			containerPath = path
			return
		}

		// It's a remote path: instance:path
		instance = potentialInstance
		containerPath = path[colonIdx+1:]
		isRemote = true
		return
	}

	// No colon found - local path
	containerPath = path
	return
}

// isWindowsPath checks if path looks like a Windows drive path (e.g., C:\...)
func isWindowsPath(path string) bool {
	if len(path) >= 2 && path[1] == ':' {
		c := path[0]
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	return false
}

// sanitizeTarPath validates and sanitizes a tar entry path to prevent path traversal attacks.
// Returns the sanitized target path or an error if the path is malicious.
// Uses path package (not filepath) because tar paths and guest paths use forward slashes.
func sanitizeTarPath(basePath, entryName string) (string, error) {
	// Clean the entry name using path.Clean (forward slashes for guest/tar paths)
	clean := path.Clean(entryName)

	// Reject absolute paths (Linux paths start with /)
	if strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("invalid tar entry: absolute path not allowed: %s", entryName)
	}

	// Reject paths that start with .. (escaping destination)
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("invalid tar entry: path escapes destination: %s", entryName)
	}

	// Join with base path using path.Join (forward slashes for guest paths)
	targetPath := path.Join(basePath, clean)

	// Verify the result is under the base path
	// path.Clean removes trailing slashes, so compare cleaned versions
	cleanBase := path.Clean(basePath)
	// Special case: if basePath is "/" (root), any absolute path under it is valid
	if cleanBase == "/" {
		// For root destination, just ensure the target is an absolute path (which path.Join guarantees)
		if !strings.HasPrefix(targetPath, "/") {
			return "", fmt.Errorf("invalid tar entry: path escapes destination: %s", entryName)
		}
	} else if !strings.HasPrefix(targetPath, cleanBase+"/") && targetPath != cleanBase {
		return "", fmt.Errorf("invalid tar entry: path escapes destination: %s", entryName)
	}

	return targetPath, nil
}

// statGuestPath queries the guest for information about a path using the SDK's Stat endpoint
func statGuestPath(ctx context.Context, client *hypeman.Client, instanceID, guestPath string, followLinks bool) (*hypeman.PathInfo, error) {
	params := hypeman.InstanceStatParams{
		Path: guestPath,
	}
	if followLinks {
		params.FollowLinks = hypeman.Bool(true)
	}

	pathInfo, err := client.Instances.Stat(ctx, instanceID, params)
	if err != nil {
		return nil, fmt.Errorf("stat path: %w", err)
	}

	// Check for stat errors (e.g., permission denied)
	if pathInfo.Error != "" {
		return nil, fmt.Errorf("stat path %s: %s", guestPath, pathInfo.Error)
	}

	return pathInfo, nil
}

// resolveDestPath resolves the destination path following docker cp semantics
// srcPath is the local source path, dstPath is the guest destination path
// Returns the resolved guest path
func resolveDestPath(ctx context.Context, client *hypeman.Client, instanceID, srcPath, dstPath string) (string, error) {
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return "", fmt.Errorf("cannot stat source: %w", err)
	}

	// Check if dstPath ends with /. (copy contents only)
	// Handle both OS-specific separator and forward slash for cross-platform compatibility
	copyContentsOnly := strings.HasSuffix(srcPath, string(filepath.Separator)+".") ||
		strings.HasSuffix(srcPath, "/.")
	if copyContentsOnly {
		srcPath = strings.TrimSuffix(srcPath, string(filepath.Separator)+".")
		srcPath = strings.TrimSuffix(srcPath, "/.")
	}

	// Check if destination ends with /
	dstEndsWithSlash := strings.HasSuffix(dstPath, "/")

	// Stat the destination in guest
	dstStat, err := statGuestPath(ctx, client, instanceID, dstPath, true)
	if err != nil {
		return "", fmt.Errorf("stat destination: %w", err)
	}

	// Use bool fields directly from PathInfo
	isDir := dstStat.IsDir
	isFile := dstStat.IsFile

	// Docker cp path resolution rules:
	// 1. If SRC is a file:
	//    - DEST doesn't exist: save as DEST
	//    - DEST doesn't exist and ends with /: error
	//    - DEST exists and is a file: overwrite
	//    - DEST exists and is a dir: copy into dir using basename
	// 2. If SRC is a directory:
	//    - DEST doesn't exist: create DEST dir
	//    - DEST exists and is a file: error
	//    - DEST exists and is a dir:
	//      - SRC ends with /.: copy contents into DEST
	//      - SRC doesn't end with /.: copy SRC dir into DEST

	if !srcInfo.IsDir() {
		// Source is a file
		if !dstStat.Exists {
			if dstEndsWithSlash {
				return "", fmt.Errorf("destination directory %s does not exist", dstPath)
			}
			// Save as DEST
			return dstPath, nil
		}
		if isDir {
			// Copy into directory using basename
			// Use path.Join for guest paths (always forward slashes)
			return path.Join(dstPath, filepath.Base(srcPath)), nil
		}
		// Overwrite file
		return dstPath, nil
	}

	// Source is a directory
	if dstStat.Exists && isFile {
		return "", fmt.Errorf("cannot copy a directory to a file")
	}

	if !dstStat.Exists {
		// DEST will be created
		return dstPath, nil
	}

	// DEST exists and is a directory
	if copyContentsOnly {
		// Copy contents into DEST
		return dstPath, nil
	}

	// Copy SRC dir into DEST (create subdir)
	// Use path.Join for guest paths (always forward slashes)
	return path.Join(dstPath, filepath.Base(srcPath)), nil
}

// buildCpWsURL builds the WebSocket URL for the cp endpoint
func buildCpWsURL(baseURL, instanceID string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}
	u.Path = fmt.Sprintf("/instances/%s/cp", instanceID)

	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}

	return u.String(), nil
}

// copyToInstance copies a local file/directory to the instance
func copyToInstance(ctx context.Context, client *hypeman.Client, baseURL, apiKey, instanceID, srcPath, dstPath string, quiet, archive, followLinks bool) error {
	// Check for /. suffix (copy contents only)
	copyContentsOnly := strings.HasSuffix(srcPath, string(filepath.Separator)+".") || strings.HasSuffix(srcPath, "/.")
	originalSrcPath := srcPath
	if copyContentsOnly {
		srcPath = strings.TrimSuffix(srcPath, string(filepath.Separator)+".")
		srcPath = strings.TrimSuffix(srcPath, "/.")
	}

	// Stat the source
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("cannot stat source: %w", err)
	}

	// Resolve destination path using docker cp semantics
	resolvedDst, err := resolveDestPath(ctx, client, instanceID, originalSrcPath, dstPath)
	if err != nil {
		return err
	}

	if srcInfo.IsDir() {
		if copyContentsOnly {
			// Copy contents of srcPath into resolvedDst
			return copyDirContentsToInstance(ctx, baseURL, apiKey, instanceID, srcPath, resolvedDst, quiet, archive, followLinks)
		}
		return copyDirToInstance(ctx, baseURL, apiKey, instanceID, srcPath, resolvedDst, quiet, archive, followLinks)
	}
	return copyFileToInstance(ctx, baseURL, apiKey, instanceID, srcPath, resolvedDst, srcInfo.Mode().Perm(), quiet, archive, followLinks)
}

// copyFileToInstance copies a single file to the instance using the SDK
func copyFileToInstance(ctx context.Context, baseURL, apiKey, instanceID, srcPath, dstPath string, mode fs.FileMode, quiet, archive, followLinks bool) error {
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	cfg := lib.CpConfig{
		BaseURL: baseURL,
		APIKey:  apiKey,
	}

	var callbacks *lib.CpCallbacks
	if !quiet {
		callbacks = &lib.CpCallbacks{
			OnFileEnd: func(path string) {
				fmt.Printf("Copied %s -> %s (%d bytes)\n", srcPath, dstPath, srcInfo.Size())
			},
		}
	}

	err = lib.CpToInstance(ctx, cfg, lib.CpToInstanceOptions{
		InstanceID:  instanceID,
		SrcPath:     srcPath,
		DstPath:     dstPath,
		Mode:        mode,
		Archive:     archive,
		FollowLinks: followLinks,
		Callbacks:   callbacks,
	})
	if err != nil {
		return err
	}

	return nil
}

// copyDirToInstance copies a directory recursively to the instance using the SDK
func copyDirToInstance(ctx context.Context, baseURL, apiKey, instanceID, srcPath, dstPath string, quiet, archive, followLinks bool) error {
	cfg := lib.CpConfig{
		BaseURL: baseURL,
		APIKey:  apiKey,
	}

	var callbacks *lib.CpCallbacks
	if !quiet {
		callbacks = &lib.CpCallbacks{
			OnFileEnd: func(path string) {
				fmt.Printf("Copied %s\n", path)
			},
		}
	}

	// First create the destination directory
	err := lib.CpToInstance(ctx, cfg, lib.CpToInstanceOptions{
		InstanceID:  instanceID,
		SrcPath:     srcPath,
		DstPath:     dstPath,
		Archive:     archive,
		FollowLinks: followLinks,
		Callbacks:   callbacks,
	})
	if err != nil {
		return err
	}

	return nil
}

// copyDirContentsToInstance copies only the contents of a directory (not the directory itself)
// This implements the /. suffix behavior from docker cp
func copyDirContentsToInstance(ctx context.Context, baseURL, apiKey, instanceID, srcPath, dstPath string, quiet, archive, followLinks bool) error {
	entries, err := os.ReadDir(srcPath)
	if err != nil {
		return fmt.Errorf("read directory: %w", err)
	}

	cfg := lib.CpConfig{
		BaseURL: baseURL,
		APIKey:  apiKey,
	}

	var callbacks *lib.CpCallbacks
	if !quiet {
		callbacks = &lib.CpCallbacks{
			OnFileEnd: func(path string) {
				fmt.Printf("Copied %s\n", path)
			},
		}
	}

	for _, entry := range entries {
		srcEntryPath := filepath.Join(srcPath, entry.Name())
		// Use path.Join for guest paths (always forward slashes)
		dstEntryPath := path.Join(dstPath, entry.Name())

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("info: %w", err)
		}

		if err := lib.CpToInstance(ctx, cfg, lib.CpToInstanceOptions{
			InstanceID:  instanceID,
			SrcPath:     srcEntryPath,
			DstPath:     dstEntryPath,
			Mode:        info.Mode().Perm(),
			Archive:     archive,
			FollowLinks: followLinks,
			Callbacks:   callbacks,
		}); err != nil {
			return err
		}
	}
	return nil
}


// createDirOnInstanceWithUidGid creates a directory on the instance with explicit uid/gid
func createDirOnInstanceWithUidGid(ctx context.Context, baseURL, apiKey, instanceID, dstPath string, mode fs.FileMode, uid, gid uint32) error {
	wsURL, err := buildCpWsURL(baseURL, instanceID)
	if err != nil {
		return err
	}

	headers := http.Header{}
	headers.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	dialer := &websocket.Dialer{}
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

	req := cpRequest{
		Direction: "to",
		GuestPath: dstPath,
		IsDir:     true,
		Mode:      uint32(mode),
		Uid:       uid,
		Gid:       gid,
	}
	reqJSON, _ := json.Marshal(req)
	if err := ws.WriteMessage(websocket.TextMessage, reqJSON); err != nil {
		return fmt.Errorf("send request: %w", err)
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

	var result cpResult
	if err := json.Unmarshal(message, &result); err != nil {
		return fmt.Errorf("parse result: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("create directory failed: %s", result.Error)
	}

	return nil
}

// copyFromInstance copies a file/directory from the instance to local using the SDK
func copyFromInstance(ctx context.Context, client *hypeman.Client, baseURL, apiKey, instanceID, srcPath, dstPath string, followLinks, quiet, archive bool) error {
	// Check for /. suffix (copy contents only) on guest source path
	copyContentsOnly := strings.HasSuffix(srcPath, "/.")
	if copyContentsOnly {
		srcPath = strings.TrimSuffix(srcPath, "/.")
	}

	// Check if destination ends with /
	dstEndsWithSlash := strings.HasSuffix(dstPath, "/") || strings.HasSuffix(dstPath, string(filepath.Separator))

	// Stat the guest source to check if it's file or directory
	srcStat, err := statGuestPath(ctx, client, instanceID, srcPath, followLinks)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
	if !srcStat.Exists {
		return fmt.Errorf("source path %s does not exist in guest", srcPath)
	}

	// Use bool field directly from PathInfo
	srcIsDir := srcStat.IsDir

	// Stat the local destination
	dstInfo, dstErr := os.Stat(dstPath)
	dstExists := dstErr == nil
	dstIsDir := dstExists && dstInfo.IsDir()

	// Apply docker cp path resolution for "from" direction
	resolvedDst := dstPath
	if !srcIsDir {
		// Source is a file
		if !dstExists {
			if dstEndsWithSlash {
				return fmt.Errorf("destination directory %s does not exist", dstPath)
			}
			// Will create file at dstPath
		} else if dstIsDir {
			// Copy into directory using basename
			// Use path.Base for guest srcPath (always forward slashes)
			resolvedDst = filepath.Join(dstPath, path.Base(srcPath))
		}
		// else: overwrite existing file
	} else {
		// Source is a directory
		if dstExists && !dstIsDir {
			return fmt.Errorf("cannot copy a directory to a file")
		}
		if !dstExists {
			// Create destination directory - will be created by SDK
		} else if !copyContentsOnly {
			// Copy SRC dir into DST - create source directory inside destination
			// Use path.Base for guest srcPath (always forward slashes)
			resolvedDst = filepath.Join(dstPath, path.Base(srcPath))
		}
		// else: copyContentsOnly=true - contents go directly into dstPath
	}
	dstPath = resolvedDst

	cfg := lib.CpConfig{
		BaseURL: baseURL,
		APIKey:  apiKey,
	}

	var fileCount int
	var totalBytes int64
	var callbacks *lib.CpCallbacks
	if !quiet {
		callbacks = &lib.CpCallbacks{
			OnFileEnd: func(path string) {
				fileCount++
			},
			OnProgress: func(bytesCopied int64) {
				totalBytes = bytesCopied
			},
		}
	}

	err = lib.CpFromInstance(ctx, cfg, lib.CpFromInstanceOptions{
		InstanceID:  instanceID,
		SrcPath:     srcPath,
		DstPath:     dstPath,
		FollowLinks: followLinks,
		Archive:     archive,
		Callbacks:   callbacks,
	})
	if err != nil {
		return err
	}

	if !quiet {
		fmt.Printf("Copied %s -> %s (%d files, %d bytes)\n", srcPath, dstPath, fileCount, totalBytes)
	}

	return nil
}

// copyFromStdinToInstance reads a tar archive from stdin and extracts it to the instance
func copyFromStdinToInstance(ctx context.Context, baseURL, apiKey, instanceID, dstPath string, archive bool) error {
	tr := tar.NewReader(os.Stdin)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break // End of tar archive
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		// Sanitize tar entry path to prevent path traversal attacks
		targetPath, err := sanitizeTarPath(dstPath, header.Name)
		if err != nil {
			return err
		}

		// Extract uid/gid from tar header if archive mode
		var uid, gid uint32
		if archive {
			uid = uint32(header.Uid)
			gid = uint32(header.Gid)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			// Create directory
			if err := createDirOnInstanceWithUidGid(ctx, baseURL, apiKey, instanceID, targetPath, fs.FileMode(header.Mode), uid, gid); err != nil {
				return fmt.Errorf("create directory %s: %w", targetPath, err)
			}

		case tar.TypeReg:
			// Copy file by reading from tar and streaming to instance
			if err := copyTarFileToInstance(ctx, baseURL, apiKey, instanceID, tr, targetPath, fs.FileMode(header.Mode), header.Size, uid, gid); err != nil {
				return fmt.Errorf("copy file %s: %w", targetPath, err)
			}

		case tar.TypeSymlink:
			// TODO: Handle symlinks if needed
			fmt.Fprintf(os.Stderr, "Warning: skipping symlink %s -> %s\n", header.Name, header.Linkname)
		}
	}

	return nil
}

// copyTarFileToInstance copies a single file from a tar reader to the instance
func copyTarFileToInstance(ctx context.Context, baseURL, apiKey, instanceID string, reader io.Reader, dstPath string, mode fs.FileMode, size int64, uid, gid uint32) error {
	wsURL, err := buildCpWsURL(baseURL, instanceID)
	if err != nil {
		return err
	}

	headers := http.Header{}
	headers.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	dialer := &websocket.Dialer{}
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
		Direction: "to",
		GuestPath: dstPath,
		IsDir:     false,
		Mode:      uint32(mode),
		Uid:       uid,
		Gid:       gid,
	}
	reqJSON, _ := json.Marshal(req)
	if err := ws.WriteMessage(websocket.TextMessage, reqJSON); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	// Stream file content from tar reader
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			if sendErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); sendErr != nil {
				return fmt.Errorf("send data: %w", sendErr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
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

	var result cpResult
	if err := json.Unmarshal(message, &result); err != nil {
		return fmt.Errorf("parse result: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("copy failed: %s", result.Error)
	}

	return nil
}

// copyFromInstanceToStdout copies files from the instance and writes a tar archive to stdout
func copyFromInstanceToStdout(ctx context.Context, baseURL, apiKey, instanceID, srcPath string, followLinks, archive bool) error {
	wsURL, err := buildCpWsURL(baseURL, instanceID)
	if err != nil {
		return err
	}

	headers := http.Header{}
	headers.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	dialer := &websocket.Dialer{}
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
		GuestPath:   srcPath,
		FollowLinks: followLinks,
	}
	reqJSON, _ := json.Marshal(req)
	if err := ws.WriteMessage(websocket.TextMessage, reqJSON); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	// Create tar writer for stdout
	tw := tar.NewWriter(os.Stdout)
	defer tw.Close()

	var currentHeader *cpFileHeader
	var bytesWritten int64
	var receivedFinal bool

	for {
		msgType, message, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				break
			}
			return fmt.Errorf("read message: %w", err)
		}

		if msgType == websocket.TextMessage {
			var msgMap map[string]interface{}
			if err := json.Unmarshal(message, &msgMap); err != nil {
				return fmt.Errorf("parse message: %w", err)
			}

			msgTypeStr, _ := msgMap["type"].(string)

			switch msgTypeStr {
			case "header":
				// Verify previous file was completely written
				if currentHeader != nil && !currentHeader.IsDir && !currentHeader.IsSymlink {
					if bytesWritten != currentHeader.Size {
						return fmt.Errorf("file %s: expected %d bytes, got %d", currentHeader.Path, currentHeader.Size, bytesWritten)
					}
				}

				var header cpFileHeader
				if err := json.Unmarshal(message, &header); err != nil {
					return fmt.Errorf("parse header: %w", err)
				}
				currentHeader = &header
				bytesWritten = 0

				if header.IsDir {
					// Write directory entry to tar
					tarHeader := &tar.Header{
						Typeflag: tar.TypeDir,
						Name:     header.Path + "/",
						Mode:     int64(header.Mode),
						ModTime:  time.Unix(header.Mtime, 0),
					}
					// Only preserve UID/GID in archive mode
					if archive {
						tarHeader.Uid = int(header.Uid)
						tarHeader.Gid = int(header.Gid)
					}
					if err := tw.WriteHeader(tarHeader); err != nil {
						return fmt.Errorf("write tar dir header: %w", err)
					}
				} else if header.IsSymlink {
					// Write symlink entry to tar
					tarHeader := &tar.Header{
						Typeflag: tar.TypeSymlink,
						Name:     header.Path,
						Linkname: header.LinkTarget,
						Mode:     int64(header.Mode),
						ModTime:  time.Unix(header.Mtime, 0),
					}
					// Only preserve UID/GID in archive mode
					if archive {
						tarHeader.Uid = int(header.Uid)
						tarHeader.Gid = int(header.Gid)
					}
					if err := tw.WriteHeader(tarHeader); err != nil {
						return fmt.Errorf("write tar symlink header: %w", err)
					}
				} else {
					// Write regular file header with known size - enables streaming
					tarHeader := &tar.Header{
						Typeflag: tar.TypeReg,
						Name:     header.Path,
						Size:     header.Size,
						Mode:     int64(header.Mode),
						ModTime:  time.Unix(header.Mtime, 0),
					}
					// Only preserve UID/GID in archive mode
					if archive {
						tarHeader.Uid = int(header.Uid)
						tarHeader.Gid = int(header.Gid)
					}
					if err := tw.WriteHeader(tarHeader); err != nil {
						return fmt.Errorf("write tar header: %w", err)
					}
				}

			case "end":
				// Verify file was completely written
				if currentHeader != nil && !currentHeader.IsDir && !currentHeader.IsSymlink {
					if bytesWritten != currentHeader.Size {
						return fmt.Errorf("file %s: expected %d bytes, got %d", currentHeader.Path, currentHeader.Size, bytesWritten)
					}
				}
				currentHeader = nil

				var endMarker cpEndMarker
				json.Unmarshal(message, &endMarker)
				if endMarker.Final {
					receivedFinal = true
					return nil
				}

			case "error":
				var cpErr cpError
				json.Unmarshal(message, &cpErr)
				return fmt.Errorf("copy error at %s: %s", cpErr.Path, cpErr.Message)
			}
		} else if msgType == websocket.BinaryMessage {
			// Stream file data directly to tar archive
			n, err := tw.Write(message)
			if err != nil {
				return fmt.Errorf("write tar data: %w", err)
			}
			bytesWritten += int64(n)
		}
	}

	// If connection closed without receiving final marker, the transfer was incomplete
	if !receivedFinal {
		return fmt.Errorf("copy stream ended without completion marker")
	}
	return nil
}


