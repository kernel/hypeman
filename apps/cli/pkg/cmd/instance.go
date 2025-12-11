// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/onkernel/hypeman-cli/internal/apiquery"
	"github.com/onkernel/hypeman-cli/internal/requestflag"
	"github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/option"
	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v3"
)

var instancesCreate = cli.Command{
	Name:  "create",
	Usage: "Create and start instance",
	Flags: []cli.Flag{
		&requestflag.StringFlag{
			Name:  "image",
			Usage: "OCI image reference",
			Config: requestflag.RequestConfig{
				BodyPath: "image",
			},
		},
		&requestflag.StringFlag{
			Name:  "name",
			Usage: "Human-readable name (lowercase letters, digits, and dashes only; cannot start or end with a dash)",
			Config: requestflag.RequestConfig{
				BodyPath: "name",
			},
		},
		&requestflag.YAMLFlag{
			Name:  "env",
			Usage: "Environment variables",
			Config: requestflag.RequestConfig{
				BodyPath: "env",
			},
		},
		&requestflag.StringFlag{
			Name:  "hotplug-size",
			Usage: `Additional memory for hotplug (human-readable format like "3GB", "1G")`,
			Value: requestflag.Value[string]("3GB"),
			Config: requestflag.RequestConfig{
				BodyPath: "hotplug_size",
			},
		},
		&requestflag.YAMLFlag{
			Name:  "network",
			Usage: "Network configuration for the instance",
			Config: requestflag.RequestConfig{
				BodyPath: "network",
			},
		},
		&requestflag.StringFlag{
			Name:  "overlay-size",
			Usage: `Writable overlay disk size (human-readable format like "10GB", "50G")`,
			Value: requestflag.Value[string]("10GB"),
			Config: requestflag.RequestConfig{
				BodyPath: "overlay_size",
			},
		},
		&requestflag.StringFlag{
			Name:  "size",
			Usage: `Base memory size (human-readable format like "1GB", "512MB", "2G")`,
			Value: requestflag.Value[string]("1GB"),
			Config: requestflag.RequestConfig{
				BodyPath: "size",
			},
		},
		&requestflag.IntFlag{
			Name:  "vcpus",
			Usage: "Number of virtual CPUs",
			Value: requestflag.Value[int64](2),
			Config: requestflag.RequestConfig{
				BodyPath: "vcpus",
			},
		},
		&requestflag.YAMLSliceFlag{
			Name:  "volume",
			Usage: "Volumes to attach to the instance at creation time",
			Config: requestflag.RequestConfig{
				BodyPath: "volumes",
			},
		},
	},
	Action:          handleInstancesCreate,
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
		&requestflag.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesDelete,
	HideHelpCommand: true,
}

var instancesGet = cli.Command{
	Name:  "get",
	Usage: "Get instance details",
	Flags: []cli.Flag{
		&requestflag.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesGet,
	HideHelpCommand: true,
}

var instancesLogs = cli.Command{
	Name:  "logs",
	Usage: "Streams instance console logs as Server-Sent Events. Returns the last N lines\n(controlled by `tail` parameter), then optionally continues streaming new lines\nif `follow=true`.",
	Flags: []cli.Flag{
		&requestflag.StringFlag{
			Name: "id",
		},
		&requestflag.BoolFlag{
			Name:  "follow",
			Usage: "Continue streaming new lines after initial output",
			Config: requestflag.RequestConfig{
				QueryPath: "follow",
			},
		},
		&requestflag.IntFlag{
			Name:  "tail",
			Usage: "Number of lines to return from end",
			Value: requestflag.Value[int64](100),
			Config: requestflag.RequestConfig{
				QueryPath: "tail",
			},
		},
	},
	Action:          handleInstancesLogs,
	HideHelpCommand: true,
}

var instancesRestore = cli.Command{
	Name:  "restore",
	Usage: "Restore instance from standby",
	Flags: []cli.Flag{
		&requestflag.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesRestore,
	HideHelpCommand: true,
}

var instancesStandby = cli.Command{
	Name:  "standby",
	Usage: "Put instance in standby (pause, snapshot, delete VMM)",
	Flags: []cli.Flag{
		&requestflag.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesStandby,
	HideHelpCommand: true,
}

var instancesStart = cli.Command{
	Name:  "start",
	Usage: "Start a stopped instance",
	Flags: []cli.Flag{
		&requestflag.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesStart,
	HideHelpCommand: true,
}

var instancesStop = cli.Command{
	Name:  "stop",
	Usage: "Stop instance (graceful shutdown)",
	Flags: []cli.Flag{
		&requestflag.StringFlag{
			Name: "id",
		},
	},
	Action:          handleInstancesStop,
	HideHelpCommand: true,
}

func handleInstancesCreate(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()

	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceNewParams{}

	options, err := flagOptions(
		cmd,
		apiquery.NestedQueryFormatBrackets,
		apiquery.ArrayQueryFormatComma,
		ApplicationJSON,
	)
	if err != nil {
		return err
	}

	var res []byte
	options = append(options, option.WithResponseBodyInto(&res))
	_, err = client.Instances.New(ctx, params, options...)
	if err != nil {
		return err
	}

	obj := gjson.ParseBytes(res)
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON(os.Stdout, "instances create", obj, format, transform)
}

func handleInstancesList(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()

	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	options, err := flagOptions(
		cmd,
		apiquery.NestedQueryFormatBrackets,
		apiquery.ArrayQueryFormatComma,
		ApplicationJSON,
	)
	if err != nil {
		return err
	}

	var res []byte
	options = append(options, option.WithResponseBodyInto(&res))
	_, err = client.Instances.List(ctx, options...)
	if err != nil {
		return err
	}

	obj := gjson.ParseBytes(res)
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON(os.Stdout, "instances list", obj, format, transform)
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
	options, err := flagOptions(
		cmd,
		apiquery.NestedQueryFormatBrackets,
		apiquery.ArrayQueryFormatComma,
		ApplicationJSON,
	)
	if err != nil {
		return err
	}

	return client.Instances.Delete(ctx, requestflag.CommandRequestValue[string](cmd, "id"), options...)
}

func handleInstancesGet(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	options, err := flagOptions(
		cmd,
		apiquery.NestedQueryFormatBrackets,
		apiquery.ArrayQueryFormatComma,
		ApplicationJSON,
	)
	if err != nil {
		return err
	}

	var res []byte
	options = append(options, option.WithResponseBodyInto(&res))
	_, err = client.Instances.Get(ctx, requestflag.CommandRequestValue[string](cmd, "id"), options...)
	if err != nil {
		return err
	}

	obj := gjson.ParseBytes(res)
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON(os.Stdout, "instances get", obj, format, transform)
}

func handleInstancesLogs(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceLogsParams{}

	options, err := flagOptions(
		cmd,
		apiquery.NestedQueryFormatBrackets,
		apiquery.ArrayQueryFormatComma,
		ApplicationJSON,
	)
	if err != nil {
		return err
	}

	stream := client.Instances.LogsStreaming(
		ctx,
		requestflag.CommandRequestValue[string](cmd, "id"),
		params,
		options...,
	)
	defer stream.Close()
	for stream.Next() {
		fmt.Printf("%s\n", stream.Current())
	}
	return stream.Err()
}

func handleInstancesRestore(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	options, err := flagOptions(
		cmd,
		apiquery.NestedQueryFormatBrackets,
		apiquery.ArrayQueryFormatComma,
		ApplicationJSON,
	)
	if err != nil {
		return err
	}

	var res []byte
	options = append(options, option.WithResponseBodyInto(&res))
	_, err = client.Instances.Restore(ctx, requestflag.CommandRequestValue[string](cmd, "id"), options...)
	if err != nil {
		return err
	}

	obj := gjson.ParseBytes(res)
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON(os.Stdout, "instances restore", obj, format, transform)
}

func handleInstancesStandby(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	options, err := flagOptions(
		cmd,
		apiquery.NestedQueryFormatBrackets,
		apiquery.ArrayQueryFormatComma,
		ApplicationJSON,
	)
	if err != nil {
		return err
	}

	var res []byte
	options = append(options, option.WithResponseBodyInto(&res))
	_, err = client.Instances.Standby(ctx, requestflag.CommandRequestValue[string](cmd, "id"), options...)
	if err != nil {
		return err
	}

	obj := gjson.ParseBytes(res)
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON(os.Stdout, "instances standby", obj, format, transform)
}

func handleInstancesStart(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	options, err := flagOptions(
		cmd,
		apiquery.NestedQueryFormatBrackets,
		apiquery.ArrayQueryFormatComma,
		ApplicationJSON,
	)
	if err != nil {
		return err
	}
	var res []byte
	options = append(options, option.WithResponseBodyInto(&res))
	_, err = client.Instances.Start(
		ctx,
		requestflag.CommandRequestValue[string](cmd, "id"),
		options...,
	)
	if err != nil {
		return err
	}

	json := gjson.Parse(string(res))
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON("instances start", json, format, transform)
}

func handleInstancesStop(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	options, err := flagOptions(
		cmd,
		apiquery.NestedQueryFormatBrackets,
		apiquery.ArrayQueryFormatComma,
		ApplicationJSON,
	)
	if err != nil {
		return err
	}
	var res []byte
	options = append(options, option.WithResponseBodyInto(&res))
	_, err = client.Instances.Stop(
		ctx,
		requestflag.CommandRequestValue[string](cmd, "id"),
		options...,
	)
	if err != nil {
		return err
	}

	json := gjson.Parse(string(res))
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON("instances stop", json, format, transform)
}
