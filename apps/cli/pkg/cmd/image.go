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

var imagesCreate = cli.Command{
	Name:  "create",
	Usage: "Pull and convert OCI image",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "name",
			Usage: "OCI image reference (e.g., docker.io/library/nginx:latest)",
		},
	},
	Action:          handleImagesCreate,
	HideHelpCommand: true,
}

var imagesRetrieve = cli.Command{
	Name:  "retrieve",
	Usage: "Get image details",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "name",
		},
	},
	Action:          handleImagesRetrieve,
	HideHelpCommand: true,
}

var imagesList = cli.Command{
	Name:            "list",
	Usage:           "List images",
	Flags:           []cli.Flag{},
	Action:          handleImagesList,
	HideHelpCommand: true,
}

var imagesDelete = cli.Command{
	Name:  "delete",
	Usage: "Delete image",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "name",
		},
	},
	Action:          handleImagesDelete,
	HideHelpCommand: true,
}

func handleImagesCreate(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	params := hypeman.ImageNewParams{}
	if err := unmarshalStdinWithFlags(cmd, map[string]string{
		"name": "name",
	}, &params); err != nil {
		return err
	}
	var res []byte
	_, err := client.Images.New(
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
	return ShowJSON("images create", json, format, transform)
}

func handleImagesRetrieve(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("name") && len(unusedArgs) > 0 {
		cmd.Set("name", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := client.Images.Get(
		ctx,
		cmd.Value("name").(string),
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
		option.WithResponseBodyInto(&res),
	)
	if err != nil {
		return err
	}

	json := gjson.Parse(string(res))
	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")
	return ShowJSON("images retrieve", json, format, transform)
}

func handleImagesList(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := client.Images.List(
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
	return ShowJSON("images list", json, format, transform)
}

func handleImagesDelete(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)
	unusedArgs := cmd.Args().Slice()
	if !cmd.IsSet("name") && len(unusedArgs) > 0 {
		cmd.Set("name", unusedArgs[0])
		unusedArgs = unusedArgs[1:]
	}
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	return client.Images.Delete(
		ctx,
		cmd.Value("name").(string),
		option.WithMiddleware(debugMiddleware(cmd.Bool("debug"))),
	)
}
