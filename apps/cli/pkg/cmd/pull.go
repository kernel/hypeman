package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/option"
	"github.com/urfave/cli/v3"
)

var pullCmd = cli.Command{
	Name:      "pull",
	Usage:     "Pull an image from a registry",
	ArgsUsage: "<image>",
	Action:    handlePull,
	HideHelpCommand: true,
}

func handlePull(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("image reference required\nUsage: hypeman pull <image>")
	}

	image := args[0]

	fmt.Fprintf(os.Stderr, "Pulling %s...\n", image)

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	params := hypeman.ImageNewParams{
		Name: image,
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	result, err := client.Images.New(
		ctx,
		params,
		opts...,
	)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Status: %s\n", result.Status)
	if result.Digest != "" {
		fmt.Fprintf(os.Stderr, "Digest: %s\n", result.Digest)
	}
	fmt.Fprintf(os.Stderr, "Image: %s\n", result.Name)

	return nil
}

