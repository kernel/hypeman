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

var instancesVolumesAttach = cli.Command{
	Name:  "attach",
	Usage: "Attach volume to instance",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
		&cli.StringFlag{
			Name: "volume-id",
		},
		&jsonflag.JSONStringFlag{
			Name:  "mount-path",
			Usage: "Path where volume should be mounted",
			Config: jsonflag.JSONConfig{
				Kind: jsonflag.Body,
				Path: "mount_path",
			},
		},
		&jsonflag.JSONBoolFlag{
			Name:  "readonly",
			Usage: "Mount as read-only",
			Config: jsonflag.JSONConfig{
				Kind:     jsonflag.Body,
				Path:     "readonly",
				SetValue: true,
			},
			Value: false,
		},
	},
	Action:          handleInstancesVolumesAttach,
	HideHelpCommand: true,
}

var instancesVolumesDetach = cli.Command{
	Name:  "detach",
	Usage: "Detach volume from instance",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "id",
		},
		&cli.StringFlag{
			Name: "volume-id",
		},
	},
	Action:          handleInstancesVolumesDetach,
	HideHelpCommand: true,
}

func handleInstancesVolumesAttach(ctx context.Context, cmd *cli.Command) error {
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("volume-id") && len(unusedArgs) > 0 {
		cmd.Set("volume-id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceVolumeAttachParams{}
	if cmd.IsSet("id") {
		params.ID = cmd.Value("id").(string)
	}
	var res []byte
	_, err := cc.client.Instances.Volumes.Attach(
		ctx,
		cmd.Value("volume-id").(string),
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
	return ShowJSON("instances:volumes attach", json, format, transform)
}

func handleInstancesVolumesDetach(ctx context.Context, cmd *cli.Command) error {
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("volume-id") && len(unusedArgs) > 0 {
		cmd.Set("volume-id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceVolumeDetachParams{}
	if cmd.IsSet("id") {
		params.ID = cmd.Value("id").(string)
	}
	var res []byte
	_, err := cc.client.Instances.Volumes.Detach(
		ctx,
		cmd.Value("volume-id").(string),
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
	return ShowJSON("instances:volumes detach", json, format, transform)
}
