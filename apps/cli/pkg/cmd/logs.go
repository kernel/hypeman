package cmd

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	"github.com/urfave/cli/v3"
)

var logsCmd = cli.Command{
	Name:      "logs",
	Usage:     "Fetch the logs of an instance",
	ArgsUsage: "<instance>",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "follow",
			Aliases: []string{"f"},
			Usage:   "Follow log output",
		},
		&cli.IntFlag{
			Name:  "tail",
			Usage: "Number of lines to show from the end of the logs",
			Value: 100,
		},
		&cli.StringFlag{
			Name:    "source",
			Aliases: []string{"s"},
			Usage:   "Log source: app (default), vmm (Cloud Hypervisor), or hypeman (operations log)",
		},
	},
	Action:          handleLogs,
	HideHelpCommand: true,
}

func handleLogs(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("instance ID required\nUsage: hypeman logs [flags] <instance>")
	}

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	// Resolve instance by ID, partial ID, or name
	instanceID, err := ResolveInstance(ctx, &client, args[0])
	if err != nil {
		return err
	}

	params := hypeman.InstanceLogsParams{}
	if cmd.IsSet("follow") {
		params.Follow = hypeman.Opt(cmd.Bool("follow"))
	}
	if cmd.IsSet("tail") {
		params.Tail = hypeman.Opt(int64(cmd.Int("tail")))
	}
	if cmd.IsSet("source") {
		params.Source = hypeman.InstanceLogsParamsSource(cmd.String("source"))
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	stream := client.Instances.LogsStreaming(
		ctx,
		instanceID,
		params,
		opts...,
	)
	defer stream.Close()

	for stream.Next() {
		fmt.Println(stream.Current())
	}

	return stream.Err()
}
