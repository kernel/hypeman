package cmd

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v3"
)

var imageCmd = cli.Command{
	Name:  "image",
	Usage: "Manage images",
	Commands: []*cli.Command{
		&imageListCmd,
		&imageGetCmd,
		&imageDeleteCmd,
	},
	HideHelpCommand: true,
}

var imageListCmd = cli.Command{
	Name:  "list",
	Usage: "List images",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Only display image names",
		},
	},
	Action:          handleImageList,
	HideHelpCommand: true,
}

var imageGetCmd = cli.Command{
	Name:      "get",
	Usage:     "Get image details",
	ArgsUsage: "<name>",
	Action:    handleImageGet,
	HideHelpCommand: true,
}

var imageDeleteCmd = cli.Command{
	Name:      "delete",
	Aliases:   []string{"rm"},
	Usage:     "Delete an image",
	ArgsUsage: "<name>",
	Action:    handleImageDelete,
	HideHelpCommand: true,
}

func handleImageList(ctx context.Context, cmd *cli.Command) error {
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
		_, err := client.Images.List(ctx, opts...)
		if err != nil {
			return err
		}
		obj := gjson.ParseBytes(res)
		return ShowJSON(os.Stdout, "image list", obj, format, transform)
	}

	images, err := client.Images.List(ctx, opts...)
	if err != nil {
		return err
	}

	quietMode := cmd.Bool("quiet")

	if quietMode {
		for _, img := range *images {
			fmt.Println(img.Name)
		}
		return nil
	}

	if len(*images) == 0 {
		fmt.Fprintln(os.Stderr, "No images found.")
		return nil
	}

	table := NewTableWriter(os.Stdout, "NAME", "STATUS", "DIGEST", "SIZE", "CREATED")
	table.TruncOrder = []int{0, 2, 4} // NAME first, then DIGEST, CREATED
	for _, img := range *images {
		digest := img.Digest
		if len(digest) > 19 {
			digest = digest[:19]
		}

		size := "-"
		if img.SizeBytes > 0 {
			size = formatBytes(img.SizeBytes)
		}

		table.AddRow(
			img.Name,
			string(img.Status),
			digest,
			size,
			FormatTimeAgo(img.CreatedAt),
		)
	}
	table.Render()

	return nil
}

func handleImageGet(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("image name required\nUsage: hypeman image get <name>")
	}

	name := args[0]

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	var res []byte
	opts = append(opts, option.WithResponseBodyInto(&res))
	_, err := client.Images.Get(ctx, url.PathEscape(name), opts...)
	if err != nil {
		return err
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	obj := gjson.ParseBytes(res)
	return ShowJSON(os.Stdout, "image get", obj, format, transform)
}

func handleImageDelete(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("image name required\nUsage: hypeman image delete <name>")
	}

	name := args[0]

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	err := client.Images.Delete(ctx, url.PathEscape(name), opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Deleted image %s\n", name)
	return nil
}
