package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v3"
)

var volumeCmd = cli.Command{
	Name:  "volume",
	Usage: "Manage volumes",
	Commands: []*cli.Command{
		&volumeCreateCmd,
		&volumeListCmd,
		&volumeGetCmd,
		&volumeDeleteCmd,
		&volumeAttachCmd,
		&volumeDetachCmd,
	},
	HideHelpCommand: true,
}

var volumeCreateCmd = cli.Command{
	Name:  "create",
	Usage: "Create a new volume",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "name",
			Usage:    "Volume name",
			Required: true,
		},
		&cli.IntFlag{
			Name:     "size",
			Usage:    "Size in gigabytes",
			Required: true,
		},
		&cli.StringFlag{
			Name:  "id",
			Usage: "Optional custom identifier (auto-generated if not provided)",
		},
	},
	Action:          handleVolumeCreate,
	HideHelpCommand: true,
}

var volumeListCmd = cli.Command{
	Name:  "list",
	Usage: "List volumes",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Only display volume IDs",
		},
	},
	Action:          handleVolumeList,
	HideHelpCommand: true,
}

var volumeGetCmd = cli.Command{
	Name:      "get",
	Usage:     "Get volume details",
	ArgsUsage: "<id>",
	Action:    handleVolumeGet,
	HideHelpCommand: true,
}

var volumeDeleteCmd = cli.Command{
	Name:      "delete",
	Aliases:   []string{"rm"},
	Usage:     "Delete a volume",
	ArgsUsage: "<id>",
	Action:    handleVolumeDelete,
	HideHelpCommand: true,
}

var volumeAttachCmd = cli.Command{
	Name:      "attach",
	Usage:     "Attach a volume to an instance",
	ArgsUsage: "<volume-id>",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "instance",
			Aliases:  []string{"i"},
			Usage:    "Instance ID or name",
			Required: true,
		},
		&cli.StringFlag{
			Name:     "mount-path",
			Usage:    "Path where volume should be mounted in the guest",
			Required: true,
		},
		&cli.BoolFlag{
			Name:  "readonly",
			Usage: "Mount as read-only",
		},
	},
	Action:          handleVolumeAttach,
	HideHelpCommand: true,
}

var volumeDetachCmd = cli.Command{
	Name:      "detach",
	Usage:     "Detach a volume from an instance",
	ArgsUsage: "<volume-id>",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "instance",
			Aliases:  []string{"i"},
			Usage:    "Instance ID or name",
			Required: true,
		},
	},
	Action:          handleVolumeDetach,
	HideHelpCommand: true,
}

func handleVolumeCreate(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	params := hypeman.VolumeNewParams{
		Name:   cmd.String("name"),
		SizeGB: int64(cmd.Int("size")),
	}

	if id := cmd.String("id"); id != "" {
		params.ID = hypeman.Opt(id)
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	var res []byte
	opts = append(opts, option.WithResponseBodyInto(&res))
	_, err := client.Volumes.New(ctx, params, opts...)
	if err != nil {
		return err
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	if format == "auto" || format == "" {
		vol := gjson.ParseBytes(res)
		fmt.Printf("%s\n", vol.Get("id").String())
		return nil
	}

	obj := gjson.ParseBytes(res)
	return ShowJSON(os.Stdout, "volume create", obj, format, transform)
}

func handleVolumeList(ctx context.Context, cmd *cli.Command) error {
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
		_, err := client.Volumes.List(ctx, opts...)
		if err != nil {
			return err
		}
		obj := gjson.ParseBytes(res)
		return ShowJSON(os.Stdout, "volume list", obj, format, transform)
	}

	volumes, err := client.Volumes.List(ctx, opts...)
	if err != nil {
		return err
	}

	quietMode := cmd.Bool("quiet")

	if quietMode {
		for _, vol := range *volumes {
			fmt.Println(vol.ID)
		}
		return nil
	}

	if len(*volumes) == 0 {
		fmt.Fprintln(os.Stderr, "No volumes found.")
		return nil
	}

	table := NewTableWriter(os.Stdout, "ID", "NAME", "SIZE", "ATTACHMENTS", "CREATED")
	table.TruncOrder = []int{0, 1, 4} // ID first, then NAME, CREATED
	for _, vol := range *volumes {
		attachments := fmt.Sprintf("%d", len(vol.Attachments))
		if len(vol.Attachments) == 0 {
			attachments = "-"
		}

		table.AddRow(
			TruncateID(vol.ID),
			vol.Name,
			fmt.Sprintf("%d GB", vol.SizeGB),
			attachments,
			FormatTimeAgo(vol.CreatedAt),
		)
	}
	table.Render()

	return nil
}

func handleVolumeGet(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("volume ID required\nUsage: hypeman volume get <id>")
	}

	id := args[0]

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	var res []byte
	opts = append(opts, option.WithResponseBodyInto(&res))
	_, err := client.Volumes.Get(ctx, id, opts...)
	if err != nil {
		return err
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	obj := gjson.ParseBytes(res)
	return ShowJSON(os.Stdout, "volume get", obj, format, transform)
}

func handleVolumeDelete(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("volume ID required\nUsage: hypeman volume delete <id>")
	}

	id := args[0]

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	err := client.Volumes.Delete(ctx, id, opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Deleted volume %s\n", id)
	return nil
}

func handleVolumeAttach(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("volume ID required\nUsage: hypeman volume attach <volume-id> --instance <instance> --mount-path <path>")
	}

	volumeID := args[0]
	instanceIdentifier := cmd.String("instance")
	mountPath := cmd.String("mount-path")

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	// Resolve instance
	instanceID, err := ResolveInstance(ctx, &client, instanceIdentifier)
	if err != nil {
		return err
	}

	params := hypeman.InstanceVolumeAttachParams{
		ID:        instanceID,
		MountPath: mountPath,
	}

	if cmd.IsSet("readonly") {
		params.Readonly = hypeman.Opt(cmd.Bool("readonly"))
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	_, err = client.Instances.Volumes.Attach(ctx, volumeID, params, opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Attached volume %s to instance %s at %s\n", volumeID, instanceIdentifier, mountPath)
	return nil
}

func handleVolumeDetach(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("volume ID required\nUsage: hypeman volume detach <volume-id> --instance <instance>")
	}

	volumeID := args[0]
	instanceIdentifier := cmd.String("instance")

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	// Resolve instance
	instanceID, err := ResolveInstance(ctx, &client, instanceIdentifier)
	if err != nil {
		return err
	}

	params := hypeman.InstanceVolumeDetachParams{
		ID: instanceID,
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	_, err = client.Instances.Volumes.Detach(ctx, volumeID, params, opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Detached volume %s from instance %s\n", volumeID, instanceIdentifier)
	return nil
}
