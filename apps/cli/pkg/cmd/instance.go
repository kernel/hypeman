// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package cmd

import (
	"context"
	"fmt"

	"github.com/onkernel/hypeman-cli/pkg/jsonflag"
	"github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/option"
	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v3"
)

var instancesCreate = cli.Command{
	Name:  "create",
	Usage: "Create and start instance",
	Flags: []cli.Flag{
		&jsonflag.JSONStringFlag{
			Name:  "id",
			Usage: "Unique identifier for the instance (provided by caller)",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "id",
			},
		},
		&jsonflag.JSONStringFlag{
			Name:  "image",
			Usage: "Image identifier",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "image",
			},
		},
		&jsonflag.JSONStringFlag{
			Name:  "name",
			Usage: "Human-readable name",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "name",
			},
		},
		&jsonflag.JSONIntFlag{
			Name:  "memory-max-mb",
			Usage: "Maximum memory with hotplug in MB",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "memory_max_mb",
			},
			Value: 4096,
		},
		&jsonflag.JSONIntFlag{
			Name:  "memory-mb",
			Usage: "Base memory in MB",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "memory_mb",
			},
			Value: 1024,
		},
		&jsonflag.JSONIntFlag{
			Name:  "port-mappings.guest_port",
			Usage: "Port mappings from host to guest",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "port_mappings.#.guest_port",
			},
		},
		&jsonflag.JSONIntFlag{
			Name:  "port-mappings.host_port",
			Usage: "Port mappings from host to guest",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "port_mappings.#.host_port",
			},
		},
		&jsonflag.JSONStringFlag{
			Name:  "port-mappings.protocol",
			Usage: "Port mappings from host to guest",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "port_mappings.#.protocol",
			},
			Value: "tcp",
		},
		&jsonflag.JSONAnyFlag{
			Name:  "+port-mapping",
			Usage: "Port mappings from host to guest",
			Config: jsonflag.JSONConfig{
				Kind:     jsonflag.Body,
				Path:     "port_mappings.-1",
				SetValue: map[string]interface{}{},
			},
		},
		&jsonflag.JSONIntFlag{
			Name:  "timeout-seconds",
			Usage: "Timeout for scale-to-zero semantics",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "timeout_seconds",
			},
			Value: 3600,
		},
		&jsonflag.JSONIntFlag{
			Name:  "vcpus",
			Usage: "Number of virtual CPUs",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "vcpus",
			},
			Value: 2,
		},
		&jsonflag.JSONStringFlag{
			Name:  "volumes.mount_path",
			Usage: "Volumes to attach",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "volumes.#.mount_path",
			},
		},
		&jsonflag.JSONStringFlag{
			Name:  "volumes.volume_id",
			Usage: "Volumes to attach",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "volumes.#.volume_id",
			},
		},
		&jsonflag.JSONBoolFlag{
			Name:  "volumes.readonly",
			Usage: "Volumes to attach",
			Config: jsonflag.JSONConfig{
				Kind:     jsonflag.Body,
				Path:     "volumes.#.readonly",
				SetValue: true,
			},
			Value: false,
		},
		&jsonflag.JSONAnyFlag{
			Name:  "+volume",
			Usage: "Volumes to attach",
			Config: jsonflag.JSONConfig{
				Kind:     jsonflag.Body,
				Path:     "volumes.-1",
				SetValue: map[string]interface{}{},
			},
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
		&jsonflag.JSONBoolFlag{
			Name:  "follow",
			Usage: "Follow logs (stream with SSE)",
			Config: jsonflag.JSONConfig{
				Kind:     jsonflag.Query,
				Path:     "follow",
				SetValue: true,
			},
			Value: false,
		},
		&jsonflag.JSONIntFlag{
			Name:  "tail",
			Usage: "Number of lines to return from end",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Query,
				Path: "tail",
			},
			Value: 100,
		},
	},
	Action:          handleInstancesStreamLogs,
	HideHelpCommand: true,
}

func handleInstancesCreate(ctx context.Context, cmd *cli.Command) error {
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceNewParams{}
	var res []byte
	_, err := cc.client.Instances.New(
		ctx,
		params,
		option.WithMiddleware(cc.AsMiddleware()),
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
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := cc.client.Instances.Get(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(cc.AsMiddleware()),
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
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := cc.client.Instances.List(
		ctx,
		option.WithMiddleware(cc.AsMiddleware()),
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
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	return cc.client.Instances.Delete(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(cc.AsMiddleware()),
	)
}

func handleInstancesPutInStandby(ctx context.Context, cmd *cli.Command) error {
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := cc.client.Instances.PutInStandby(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(cc.AsMiddleware()),
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
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := cc.client.Instances.RestoreFromStandby(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(cc.AsMiddleware()),
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
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceStreamLogsParams{}
	stream := cc.client.Instances.StreamLogsStreaming(
		ctx,
		cmd.Value("id").(string),
		params,
		option.WithMiddleware(cc.AsMiddleware()),
	)
	for stream.Next() {
		fmt.Printf("%s\n", stream.Current().RawJSON())
	}
	return stream.Err()
}
