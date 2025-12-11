package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/option"
	"github.com/urfave/cli/v3"
)

var runCmd = cli.Command{
	Name:      "run",
	Usage:     "Create and start a new instance from an image",
	ArgsUsage: "<image>",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "name",
			Usage: "Instance name (auto-generated if not provided)",
		},
		&cli.StringSliceFlag{
			Name:    "env",
			Aliases: []string{"e"},
			Usage:   "Set environment variable (KEY=VALUE, can be repeated)",
		},
		&cli.StringFlag{
			Name:  "memory",
			Usage: `Base memory size (e.g., "1GB", "512MB")`,
			Value: "1GB",
		},
		&cli.IntFlag{
			Name:  "cpus",
			Usage: "Number of virtual CPUs",
			Value: 2,
		},
		&cli.StringFlag{
			Name:  "overlay-size",
			Usage: `Writable overlay disk size (e.g., "10GB")`,
			Value: "10GB",
		},
		&cli.StringFlag{
			Name:  "hotplug-size",
			Usage: `Additional memory for hotplug (e.g., "3GB")`,
			Value: "3GB",
		},
		&cli.BoolFlag{
			Name:  "network",
			Usage: "Enable network (default: true)",
			Value: true,
		},
	},
	Action:          handleRun,
	HideHelpCommand: true,
}

func handleRun(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("image reference required\nUsage: hypeman run [flags] <image>")
	}

	image := args[0]

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	// Check if image exists and is ready
	imgInfo, err := client.Images.Get(ctx, image)
	if err != nil {
		// Image not found, try to pull it
		var apiErr *hypeman.Error
		if ok := isNotFoundError(err, &apiErr); ok {
			fmt.Fprintf(os.Stderr, "Image not found locally. Pulling %s...\n", image)
			imgInfo, err = client.Images.New(ctx, hypeman.ImageNewParams{
				Name: image,
			})
			if err != nil {
				return fmt.Errorf("failed to pull image: %w", err)
			}
		} else {
			return fmt.Errorf("failed to check image: %w", err)
		}
	}

	// Wait for image to be ready (build is asynchronous)
	if err := waitForImageReady(ctx, &client, imgInfo); err != nil {
		return err
	}

	// Generate name if not provided
	name := cmd.String("name")
	if name == "" {
		name = GenerateInstanceName(image)
	}

	// Parse environment variables
	env := make(map[string]string)
	for _, e := range cmd.StringSlice("env") {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		} else {
			fmt.Fprintf(os.Stderr, "Warning: ignoring malformed env var: %s\n", e)
		}
	}

	// Build instance params
	// Note: SDK uses memory in MB, but we accept human-readable format
	// For simplicity, we pass memory as-is and let the server handle conversion
	params := hypeman.InstanceNewParams{
		Image: image,
		Name:  name,
		Vcpus: hypeman.Opt(int64(cmd.Int("cpus"))),
	}
	if len(env) > 0 {
		params.Env = env
	}

	fmt.Fprintf(os.Stderr, "Creating instance %s...\n", name)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	result, err := client.Instances.New(
		ctx,
		params,
		opts...,
	)
	if err != nil {
		return err
	}

	// Output instance ID (useful for scripting)
	fmt.Println(result.ID)

	return nil
}

// isNotFoundError checks if err is a 404 not found error
func isNotFoundError(err error, target **hypeman.Error) bool {
	if apiErr, ok := err.(*hypeman.Error); ok {
		*target = apiErr
		return apiErr.Response != nil && apiErr.Response.StatusCode == 404
	}
	return false
}

// waitForImageReady polls image status until it becomes ready or failed
func waitForImageReady(ctx context.Context, client *hypeman.Client, img *hypeman.Image) error {
	if img.Status == hypeman.ImageStatusReady {
		return nil
	}
	if img.Status == hypeman.ImageStatusFailed {
		if img.Error != "" {
			return fmt.Errorf("image build failed: %s", img.Error)
		}
		return fmt.Errorf("image build failed")
	}

	// Poll until ready using the normalized image name from the API response
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	// Show initial status
	showImageStatus(img)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			updated, err := client.Images.Get(ctx, img.Name)
			if err != nil {
				return fmt.Errorf("failed to check image status: %w", err)
			}

			// Show status update if changed
			if updated.Status != img.Status {
				showImageStatus(updated)
				img = updated
			}

			switch updated.Status {
			case hypeman.ImageStatusReady:
				return nil
			case hypeman.ImageStatusFailed:
				if updated.Error != "" {
					return fmt.Errorf("image build failed: %s", updated.Error)
				}
				return fmt.Errorf("image build failed")
			}
		}
	}
}

// showImageStatus prints image build status to stderr
func showImageStatus(img *hypeman.Image) {
	switch img.Status {
	case hypeman.ImageStatusPending:
		if img.QueuePosition > 0 {
			fmt.Fprintf(os.Stderr, "Queued (position %d)...\n", img.QueuePosition)
		} else {
			fmt.Fprintf(os.Stderr, "Queued...\n")
		}
	case hypeman.ImageStatusPulling:
		fmt.Fprintf(os.Stderr, "Pulling image...\n")
	case hypeman.ImageStatusConverting:
		fmt.Fprintf(os.Stderr, "Converting to disk image...\n")
	case hypeman.ImageStatusReady:
		fmt.Fprintf(os.Stderr, "Image ready.\n")
	}
}
