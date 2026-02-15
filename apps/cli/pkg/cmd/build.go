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
	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v3"
)

var buildCmd = cli.Command{
	Name:      "build",
	Usage:     "Build an image from a Dockerfile",
	ArgsUsage: "[path]",
	Description: `Build an image from a Dockerfile and source context.

The path argument specifies the build context directory containing the
source code and Dockerfile. If not specified, the current directory is used.

Subcommands are available for managing builds:
  hypeman build list       List builds
  hypeman build get <id>   Get build details
  hypeman build cancel <id> Cancel a build

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
		&cli.StringFlag{
			Name:  "base-image-digest",
			Usage: "Pinned base image digest for reproducible builds",
		},
		&cli.StringFlag{
			Name:  "cache-scope",
			Usage: "Tenant-specific cache key prefix",
		},
		&cli.StringFlag{
			Name:  "global-cache-key",
			Usage: `Global cache identifier (e.g., "node", "python", "ubuntu")`,
		},
		&cli.StringFlag{
			Name:  "is-admin-build",
			Usage: `Set to "true" to grant push access to global cache (operator-only)`,
		},
		&cli.StringFlag{
			Name:  "secrets",
			Usage: `JSON array of secret references to inject during build (e.g., '[{"id":"npm_token"}]')`,
		},
		&cli.StringFlag{
			Name:  "image-name",
			Usage: `Custom image name for the build output (pushed to {registry}/{image_name} instead of {registry}/builds/{id})`,
		},
	},
	Commands: []*cli.Command{
		&buildListCmd,
		&buildGetCmd,
		&buildCancelCmd,
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

	if v := cmd.String("base-image-digest"); v != "" {
		params.BaseImageDigest = hypeman.Opt(v)
	}
	if v := cmd.String("cache-scope"); v != "" {
		params.CacheScope = hypeman.Opt(v)
	}
	if v := cmd.String("global-cache-key"); v != "" {
		params.GlobalCacheKey = hypeman.Opt(v)
	}
	if v := cmd.String("is-admin-build"); v != "" {
		params.IsAdminBuild = hypeman.Opt(v)
	}
	if v := cmd.String("secrets"); v != "" {
		params.Secrets = hypeman.Opt(v)
	}
	if v := cmd.String("image-name"); v != "" {
		params.ImageName = hypeman.Opt(v)
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

var buildListCmd = cli.Command{
	Name:  "list",
	Usage: "List builds",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Only display build IDs",
		},
	},
	Action:          handleBuildList,
	HideHelpCommand: true,
}

var buildGetCmd = cli.Command{
	Name:      "get",
	Usage:     "Get build details",
	ArgsUsage: "<id>",
	Action:    handleBuildGet,
	HideHelpCommand: true,
}

var buildCancelCmd = cli.Command{
	Name:      "cancel",
	Usage:     "Cancel a build",
	ArgsUsage: "<id>",
	Action:    handleBuildCancel,
	HideHelpCommand: true,
}

func handleBuildList(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	if format != "auto" {
		var res []byte
		opts = append(opts, option.WithResponseBodyInto(&res))
		_, err := client.Builds.List(ctx, opts...)
		if err != nil {
			return err
		}
		obj := gjson.ParseBytes(res)
		return ShowJSON(os.Stdout, "build list", obj, format, transform)
	}

	builds, err := client.Builds.List(ctx, opts...)
	if err != nil {
		return err
	}

	quietMode := cmd.Bool("quiet")

	if quietMode {
		for _, b := range *builds {
			fmt.Println(b.ID)
		}
		return nil
	}

	if len(*builds) == 0 {
		fmt.Fprintln(os.Stderr, "No builds found.")
		return nil
	}

	table := NewTableWriter(os.Stdout, "ID", "STATUS", "IMAGE", "DURATION", "CREATED")
	table.TruncOrder = []int{2, 0, 4} // IMAGE first, then ID, CREATED
	for _, b := range *builds {
		imageRef := b.ImageRef
		if imageRef == "" {
			imageRef = "-"
		}

		duration := "-"
		if b.DurationMs > 0 {
			secs := b.DurationMs / 1000
			if secs < 60 {
				duration = fmt.Sprintf("%ds", secs)
			} else {
				duration = fmt.Sprintf("%dm%ds", secs/60, secs%60)
			}
		}

		table.AddRow(
			TruncateID(b.ID),
			string(b.Status),
			imageRef,
			duration,
			FormatTimeAgo(b.CreatedAt),
		)
	}
	table.Render()

	return nil
}

func handleBuildGet(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("build ID required\nUsage: hypeman build get <id>")
	}

	id := args[0]

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	var res []byte
	opts = append(opts, option.WithResponseBodyInto(&res))
	_, err := client.Builds.Get(ctx, id, opts...)
	if err != nil {
		return err
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	obj := gjson.ParseBytes(res)
	return ShowJSON(os.Stdout, "build get", obj, format, transform)
}

func handleBuildCancel(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("build ID required\nUsage: hypeman build cancel <id>")
	}

	id := args[0]

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	err := client.Builds.Cancel(ctx, id, opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Cancelled build %s\n", id)
	return nil
}
