package cmd

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
)

// BuildEvent represents an event from the build SSE stream
type BuildEvent struct {
	Type      string `json:"type"`      // "log", "status", "heartbeat"
	Timestamp string `json:"timestamp"`
	Content   string `json:"content,omitempty"` // for type=log
	Status    string `json:"status,omitempty"`  // for type=status
}

// Build represents the build response from the API
type Build struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	ImageDigest string `json:"image_digest,omitempty"`
	ImageRef    string `json:"image_ref,omitempty"`
	Error       string `json:"error,omitempty"`
}

var buildCmd = cli.Command{
	Name:      "build",
	Usage:     "Build an image from a Dockerfile",
	ArgsUsage: "[path]",
	Description: `Build an image from a Dockerfile and source context.

The path argument specifies the build context directory containing the
source code and Dockerfile. If not specified, the current directory is used.

Examples:
  # Build from current directory
  hypeman build

  # Build from a specific directory
  hypeman build ./myapp

  # Build with a specific Dockerfile
  hypeman build -f Dockerfile.prod ./myapp

  # Build with custom timeout
  hypeman build --timeout 1200 ./myapp`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "file",
			Aliases: []string{"f"},
			Usage:   "Path to Dockerfile (relative to context or absolute)",
		},
		&cli.IntFlag{
			Name:  "timeout",
			Usage: "Build timeout in seconds",
			Value: 600,
		},
	},
	Action:          handleBuild,
	HideHelpCommand: true,
}

func handleBuild(ctx context.Context, cmd *cli.Command) error {
	// Get build context path (default to current directory)
	contextPath := "."
	args := cmd.Args().Slice()
	if len(args) > 0 {
		contextPath = args[0]
	}

	// Resolve to absolute path
	absContextPath, err := filepath.Abs(contextPath)
	if err != nil {
		return fmt.Errorf("failed to resolve path: %w", err)
	}

	// Check if context directory exists
	info, err := os.Stat(absContextPath)
	if err != nil {
		return fmt.Errorf("cannot access build context: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("build context must be a directory: %s", absContextPath)
	}

	// Get Dockerfile path
	dockerfilePath := cmd.String("file")
	var dockerfileContent []byte

	if dockerfilePath != "" {
		// If dockerfile is specified, read it
		if !filepath.IsAbs(dockerfilePath) {
			dockerfilePath = filepath.Join(absContextPath, dockerfilePath)
		}
		dockerfileContent, err = os.ReadFile(dockerfilePath)
		if err != nil {
			return fmt.Errorf("cannot read Dockerfile: %w", err)
		}
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

	timeout := cmd.Int("timeout")

	fmt.Fprintf(os.Stderr, "Building from %s...\n", contextPath)

	// Create source tarball
	tarball, err := createSourceTarball(absContextPath)
	if err != nil {
		return fmt.Errorf("failed to create source archive: %w", err)
	}

	// Upload build and get build ID
	build, err := uploadBuild(ctx, baseURL, apiKey, tarball, dockerfileContent, int(timeout))
	if err != nil {
		return fmt.Errorf("failed to start build: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Build started: %s\n", build.ID)

	// Stream build events
	err = streamBuildEvents(ctx, baseURL, apiKey, build.ID)
	if err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	return nil
}

// createSourceTarball creates a gzipped tar archive of the build context
func createSourceTarball(contextPath string) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	gzWriter := gzip.NewWriter(buf)
	tarWriter := tar.NewWriter(gzWriter)

	err := filepath.Walk(contextPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		relPath, err := filepath.Rel(contextPath, path)
		if err != nil {
			return err
		}

		// Skip root directory
		if relPath == "." {
			return nil
		}

		// Skip common build artifacts and version control
		base := filepath.Base(path)
		if base == ".git" || base == "node_modules" || base == "__pycache__" ||
			base == ".venv" || base == "venv" || base == "target" ||
			base == ".docker" || base == ".dockerignore" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}

		// Use forward slashes for tar paths
		header.Name = filepath.ToSlash(relPath)

		// Handle symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Linkname = linkTarget
		}

		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		// Write file content for regular files
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			if _, err := io.Copy(tarWriter, file); err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gzWriter.Close(); err != nil {
		return nil, err
	}

	return buf, nil
}

// uploadBuild uploads the source tarball to the builds API
func uploadBuild(ctx context.Context, baseURL, apiKey string, source *bytes.Buffer, dockerfile []byte, timeout int) (*Build, error) {
	// Create multipart form
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)

	// Add source tarball
	sourcePart, err := writer.CreateFormFile("source", "source.tar.gz")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(sourcePart, source); err != nil {
		return nil, err
	}

	// Add dockerfile if provided separately
	if dockerfile != nil {
		if err := writer.WriteField("dockerfile", string(dockerfile)); err != nil {
			return nil, err
		}
	}

	// Add timeout
	if err := writer.WriteField("timeout_seconds", fmt.Sprintf("%d", timeout)); err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/builds", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)

	// Send request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("build request failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var build Build
	if err := json.Unmarshal(respBody, &build); err != nil {
		return nil, fmt.Errorf("failed to parse build response: %w", err)
	}

	return &build, nil
}

// streamBuildEvents streams build events from the SSE endpoint
func streamBuildEvents(ctx context.Context, baseURL, apiKey, buildID string) error {
	url := fmt.Sprintf("%s/builds/%s/events?follow=true", baseURL, buildID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Cache-Control", "no-cache")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to connect to build events (HTTP %d): %s", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	var finalStatus string
	var buildError string

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		// Parse SSE data line
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)

			if data == "" {
				continue
			}

			var event BuildEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				// Skip malformed events
				continue
			}

			switch event.Type {
			case "log":
				// Print log content
				fmt.Println(event.Content)

			case "status":
				finalStatus = event.Status
				switch event.Status {
				case "queued":
					fmt.Fprintf(os.Stderr, "Build queued...\n")
				case "building":
					fmt.Fprintf(os.Stderr, "Building...\n")
				case "pushing":
					fmt.Fprintf(os.Stderr, "Pushing image...\n")
				case "ready":
					fmt.Fprintf(os.Stderr, "Build complete!\n")
					return nil
				case "failed":
					buildError = "build failed"
				case "cancelled":
					return fmt.Errorf("build was cancelled")
				}

			case "heartbeat":
				// Ignore heartbeat events
			}
		}
	}

	// Check final status
	if finalStatus == "failed" {
		return fmt.Errorf(buildError)
	}
	if finalStatus == "ready" {
		return nil
	}

	return fmt.Errorf("build stream ended unexpectedly (status: %s)", finalStatus)
}
