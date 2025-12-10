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

var instancesVolumesAttach = cli.Command{
	Name:  "attach",
	Usage: "Attach volume to instance",
	Flags: []cli.Flag{
		&requestflag.StringFlag{
			Name: "id",
		},
		&requestflag.StringFlag{
			Name: "volume-id",
		},
		&requestflag.StringFlag{
			Name:  "mount-path",
			Usage: "Path where volume should be mounted",
			Config: requestflag.RequestConfig{
				BodyPath: "mount_path",
			},
		},
		&requestflag.BoolFlag{
			Name:  "readonly",
			Usage: "Mount as read-only",
			Config: requestflag.RequestConfig{
				BodyPath: "readonly",
			},
		},
	},
	Action:          handleInstancesVolumesAttach,
	HideHelpCommand: true,
}

var instancesVolumesDetach = cli.Command{
	Name:  "detach",
	Usage: "Detach volume from instance",
	Flags: []cli.Flag{
		&requestflag.StringFlag{
			Name: "id",
		},
		&requestflag.StringFlag{
			Name: "volume-id",
		},
	},
	Action:          handleInstancesVolumesDetach,
	HideHelpCommand: true,
}

func handleInstancesVolumesAttach(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("volume-id") && len(unusedArgs) > 0 {
		cmd.Set("volume-id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceVolumeAttachParams{
		ID: requestflag.CommandRequestValue[string](cmd, "id"),
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
	_, err = client.Instances.Volumes.Attach(
		ctx,
		requestflag.CommandRequestValue[string](cmd, "volume-id"),
		params,
		options...,
	)
	if err != nil {
		return err
	}

	obj := gjson.ParseBytes(res)
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON(os.Stdout, "instances:volumes attach", obj, format, transform)
}

func handleInstancesVolumesDetach(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("volume-id") && len(unusedArgs) > 0 {
		cmd.Set("volume-id", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.InstanceVolumeDetachParams{
		ID: requestflag.CommandRequestValue[string](cmd, "id"),
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
	_, err = client.Instances.Volumes.Detach(
		ctx,
		requestflag.CommandRequestValue[string](cmd, "volume-id"),
		params,
		options...,
	)
	if err != nil {
		return err
	}

	obj := gjson.ParseBytes(res)
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON(os.Stdout, "instances:volumes detach", obj, format, transform)
}
