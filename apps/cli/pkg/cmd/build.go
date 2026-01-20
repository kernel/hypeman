package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	"github.com/urfave/cli/v3"
)

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
	var dockerfileContent string

	if dockerfilePath != "" {
		// If dockerfile is specified, read it
		if !filepath.IsAbs(dockerfilePath) {
			dockerfilePath = filepath.Join(absContextPath, dockerfilePath)
		}
		content, err := os.ReadFile(dockerfilePath)
		if err != nil {
			return fmt.Errorf("cannot read Dockerfile: %w", err)
		}
		dockerfileContent = string(content)
	}

	timeout := cmd.Int("timeout")

	fmt.Fprintf(os.Stderr, "Building from %s...\n", contextPath)

	// Create source tarball
	tarball, err := createSourceTarball(absContextPath)
	if err != nil {
		return fmt.Errorf("failed to create source archive: %w", err)
	}

	// Create client with options
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	// Build params
	params := hypeman.BuildNewParams{
		Source:         bytes.NewReader(tarball.Bytes()),
		TimeoutSeconds: hypeman.Opt(int64(timeout)),
	}

	if dockerfileContent != "" {
		params.Dockerfile = hypeman.Opt(dockerfileContent)
	}

	// Start build
	build, err := client.Builds.New(ctx, params, opts...)
	if err != nil {
		return fmt.Errorf("failed to start build: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Build started: %s\n", build.ID)

	// Stream build events
	err = streamBuildEventsSDK(ctx, client, build.ID, opts)
	if err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	return nil
}

// streamBuildEventsSDK streams build events using the SDK
func streamBuildEventsSDK(ctx context.Context, client hypeman.Client, buildID string, opts []option.RequestOption) error {
	stream := client.Builds.EventsStreaming(
		ctx,
		buildID,
		hypeman.BuildEventsParams{
			Follow: hypeman.Opt(true),
		},
		opts...,
	)
	defer stream.Close()

	var finalStatus hypeman.BuildStatus
	var buildError string

	for stream.Next() {
		event := stream.Current()

		switch event.Type {
		case hypeman.BuildEventTypeLog:
			// Print log content
			fmt.Println(event.Content)

		case hypeman.BuildEventTypeStatus:
			finalStatus = event.Status
			switch event.Status {
			case hypeman.BuildStatusQueued:
				fmt.Fprintf(os.Stderr, "Build queued...\n")
			case hypeman.BuildStatusBuilding:
				fmt.Fprintf(os.Stderr, "Building...\n")
			case hypeman.BuildStatusPushing:
				fmt.Fprintf(os.Stderr, "Pushing image...\n")
			case hypeman.BuildStatusReady:
				fmt.Fprintf(os.Stderr, "Build complete!\n")
				return nil
			case hypeman.BuildStatusFailed:
				buildError = "build failed"
			case hypeman.BuildStatusCancelled:
				return fmt.Errorf("build was cancelled")
			}

		case hypeman.BuildEventTypeHeartbeat:
			// Ignore heartbeat events
		}
	}

	if err := stream.Err(); err != nil {
		return err
	}

	// Check final status
	if finalStatus == hypeman.BuildStatusFailed {
		return fmt.Errorf("%s", buildError)
	}
	if finalStatus == hypeman.BuildStatusReady {
		return nil
	}

	return fmt.Errorf("build stream ended unexpectedly (status: %s)", finalStatus)
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
