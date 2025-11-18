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
			Name:  "id",
			Usage: "Unique identifier for the instance (provided by caller)",
		},
		&cli.StringFlag{
			Name:  "image",
			Usage: "Image identifier",
		},
		&cli.StringFlag{
			Name:  "name",
			Usage: "Human-readable name",
		},
		&cli.Int64Flag{
			Name:  "memory-max-mb",
			Usage: "Maximum memory with hotplug in MB",
			Value: 4096,
		},
		&cli.Int64Flag{
			Name:  "memory-mb",
			Usage: "Base memory in MB",
			Value: 1024,
		},
		&cli.Int64Flag{
			Name:  "timeout-seconds",
			Usage: "Timeout for scale-to-zero semantics",
			Value: 3600,
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
		"id":              "id",
		"image":           "image",
		"name":            "name",
		"memory-max-mb":   "memory_max_mb",
		"memory-mb":       "memory_mb",
		"timeout-seconds": "timeout_seconds",
		"vcpus":           "vcpus",
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
