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

var volumesCreate = cli.Command{
	Name:  "create",
	Usage: "Create volume",
	Flags: []cli.Flag{
		&jsonflag.JSONStringFlag{
			Name:  "name",
			Usage: "Volume name",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "name",
			},
		},
		&jsonflag.JSONIntFlag{
			Name:  "size-gb",
			Usage: "Size in gigabytes",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "size_gb",
			},
		},
		&jsonflag.JSONStringFlag{
			Name:  "id",
			Usage: "Optional custom identifier (auto-generated if not provided)",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "id",
			},
		},
	},
	Action:          handleVolumesCreate,
	HideHelpCommand: true,
}

var volumesRetrieve = cli.Command{
	Name:  "retrieve",
	Usage: "Get volume details",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
	},
	Action:          handleVolumesRetrieve,
	HideHelpCommand: true,
}

var volumesList = cli.Command{
	Name:            "list",
	Usage:           "List volumes",
	Flags:           []cli.Flag{},
	Action:          handleVolumesList,
	HideHelpCommand: true,
}

var volumesDelete = cli.Command{
	Name:  "delete",
	Usage: "Delete volume",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
	},
	Action:          handleVolumesDelete,
	HideHelpCommand: true,
}

func handleVolumesCreate(ctx context.Context, cmd *cli.Command) error {
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.VolumeNewParams{}
	var res []byte
	_, err := cc.client.Volumes.New(
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
	return ShowJSON("volumes create", json, format, transform)
}

func handleVolumesRetrieve(ctx context.Context, cmd *cli.Command) error {
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
	_, err := cc.client.Volumes.Get(
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
	return ShowJSON("volumes retrieve", json, format, transform)
}

func handleVolumesList(ctx context.Context, cmd *cli.Command) error {
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := cc.client.Volumes.List(
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
	return ShowJSON("volumes list", json, format, transform)
}

func handleVolumesDelete(ctx context.Context, cmd *cli.Command) error {
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("id") && len(unusedArgs) > 0 {
		cmd.Set("id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	return cc.client.Volumes.Delete(
		ctx,
		cmd.Value("id").(string),
		option.WithMiddleware(cc.AsMiddleware()),
	)
}
