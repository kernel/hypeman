// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package cmd

import (
	"context"
	"fmt"

	"github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/option"
	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v3"
)

var instancesCreate = cli.Command{
	Name:  "create",
	Usage: "Create and start instance",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "image",
			Usage: "OCI image reference",
		},
		&cli.StringFlag{
			Name:  "name",
			Usage: "Human-readable name (lowercase letters, digits, and dashes only; cannot start or end with a dash)",
		},
		&cli.StringFlag{
			Name:  "hotplug-size",
			Usage: `Additional memory for hotplug (human-readable format like "3GB", "1G")`,
			Value: "3GB",
		},
		&cli.StringFlag{
			Name:  "overlay-size",
			Usage: `Writable overlay disk size (human-readable format like "10GB", "50G")`,
			Value: "10GB",
		},
		&cli.StringFlag{
			Name:  "size",
			Usage: `Base memory size (human-readable format like "1GB", "512MB", "2G")`,
			Value: "1GB",
		},
		&cli.Int64Flag{
			Name:  "vcpus",
			Usage: "Number of virtual CPUs",
			Value: 2,
		},
	},
	Action:          handleInstancesCreate,
	HideHelpCommand: true,
}

var instancesRetrieve = cli.Command{
	Name:  "retrieve",
	Usage: "Get instance details",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesRetrieve,
	HideHelpCommand: true,
}

var instancesList = cli.Command{
	Name:            "list",
	Usage:           "List instances",
	Flags:           []cli.Flag{},
	Action:          handleInstancesList,
	HideHelpCommand: true,
}

var instancesDelete = cli.Command{
	Name:  "delete",
	Usage: "Stop and delete instance",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesDelete,
	HideHelpCommand: true,
}

var instancesExecuteCommand = cli.Command{
	Name:  "execute-command",
	Usage: "Upgrades to WebSocket for bidirectional streaming for shell access.",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesExecuteCommand,
	HideHelpCommand: true,
}

var instancesPutInStandby = cli.Command{
	Name:  "put-in-standby",
	Usage: "Put instance in standby (pause, snapshot, delete VMM)",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesPutInStandby,
	HideHelpCommand: true,
}

var instancesRestoreFromStandby = cli.Command{
	Name:  "restore-from-standby",
	Usage: "Restore instance from standby",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesRestoreFromStandby,
	HideHelpCommand: true,
}

var instancesStreamLogs = cli.Command{
	Name:  "stream-logs",
	Usage: "Stream instance logs (SSE)",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
		&cli.BoolFlag{
			Name:  "follow",
			Usage: "Follow logs (stream with SSE)",
		},
		&cli.Int64Flag{
			Name:  "tail",
			Usage: "Number of lines to return from end",
			Value: 100,
		},
	},
	Action:          handleInstancesStreamLogs,
	HideHelpCommand: true,
}

func handleInstancesCreate(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceNewParams{}
	if err := unmarshalStdinWithFlags(cmd, map[string]string{
		"image":        "image",
		"name":         "name",
		"hotplug-size": "hotplug_size",
		"overlay-size": "overlay_size",
		"size":         "size",
		"vcpus":        "vcpus",
	}, &params); err != nil {
		return err
	}
	var res []byte
	_, err := client.Instances.New(
		ctx,
		params,
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
		option.WithResponseBodyInto(&res),
	)
	if err != nil {
		return err
	}

	json := gjson.Parse(string(res))
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON("instances create", json, format, transform)
}

func handleInstancesRetrieve(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := client.Instances.Get(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
		option.WithResponseBodyInto(&res),
	)
	if err != nil {
		return err
	}

	json := gjson.Parse(string(res))
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON("instances retrieve", json, format, transform)
}

func handleInstancesList(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := client.Instances.List(
		ctx,
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
		option.WithResponseBodyInto(&res),
	)
	if err != nil {
		return err
	}

	json := gjson.Parse(string(res))
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON("instances list", json, format, transform)
}

func handleInstancesDelete(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	return client.Instances.Delete(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
	)
}

func handleInstancesExecuteCommand(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	return client.Instances.ExecuteCommand(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
	)
}

func handleInstancesPutInStandby(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := client.Instances.PutInStandby(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
		option.WithResponseBodyInto(&res),
	)
	if err != nil {
		return err
	}

	json := gjson.Parse(string(res))
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON("instances put-in-standby", json, format, transform)
}

func handleInstancesRestoreFromStandby(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := client.Instances.RestoreFromStandby(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
		option.WithResponseBodyInto(&res),
	)
	if err != nil {
		return err
	}

	json := gjson.Parse(string(res))
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON("instances restore-from-standby", json, format, transform)
}

func handleInstancesStreamLogs(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceStreamLogsParams{}
	if cmd.IsSet("follow") {
		params.Follow = hypeman.Opt(cmd.Value("follow").(bool))
	}
	if cmd.IsSet("tail") {
		params.Tail = hypeman.Opt(cmd.Value("tail").(int64))
	}
	stream := client.Instances.StreamLogsStreaming(
		ctx,
		cmd.Value("id").(string),
		params,
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
	)
	defer stream.Close()
	for stream.Next() {
		fmt.Printf("%s\n", stream.Current())
	}
	return stream.Err()
}
