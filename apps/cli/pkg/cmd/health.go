// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package cmd

import (
	"context"
	"fmt"
	"https://github.com/stainless-sdks/hypeman-go/option"

	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v3"
)

var healthCheck = cli.Command{
	Name:            "check",
	Usage:           "Health check",
	Flags:           []cli.Flag{},
	Action:          handleHealthCheck,
	HideHelpCommand: true,
}

func handleHealthCheck(ctx context.Context, cmd *cli.Command) error {
	cc := getAPICommandContext(cmd)
	unusedArgs := cmd.Args().Slice()
	if len(unusedArgs) > 0 {
		return fmt.Errorf("Unexpected extra arguments: %v", unusedArgs)
	}
	var res []byte
	_, err := cc.client.Health.Check(
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
	return ShowJSON("health check", json, format, transform)
}
