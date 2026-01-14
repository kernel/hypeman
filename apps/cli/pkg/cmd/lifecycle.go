package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	"github.com/urfave/cli/v3"
)

var stopCmd = cli.Command{
	Name:            "stop",
	Usage:           "Stop a running instance",
	ArgsUsage:       "<instance>",
	Action:          handleStop,
	HideHelpCommand: true,
}

var startCmd = cli.Command{
	Name:            "start",
	Usage:           "Start a stopped instance",
	ArgsUsage:       "<instance>",
	Action:          handleStart,
	HideHelpCommand: true,
}

var standbyCmd = cli.Command{
	Name:            "standby",
	Usage:           "Put an instance into standby (pause and snapshot)",
	ArgsUsage:       "<instance>",
	Action:          handleStandby,
	HideHelpCommand: true,
}

var restoreCmd = cli.Command{
	Name:            "restore",
	Usage:           "Restore an instance from standby",
	ArgsUsage:       "<instance>",
	Action:          handleRestore,
	HideHelpCommand: true,
}

func handleStop(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("instance name or ID required\nUsage: hypeman stop <instance>")
	}

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	instanceID, err := ResolveInstance(ctx, &client, args[0])
	if err != nil {
		return err
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	fmt.Fprintf(os.Stderr, "Stopping %s...\n", args[0])

	instance, err := client.Instances.Stop(ctx, instanceID, opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Stopped %s (state: %s)\n", instance.Name, instance.State)
	return nil
}

func handleStart(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("instance name or ID required\nUsage: hypeman start <instance>")
	}

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	instanceID, err := ResolveInstance(ctx, &client, args[0])
	if err != nil {
		return err
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	fmt.Fprintf(os.Stderr, "Starting %s...\n", args[0])

	instance, err := client.Instances.Start(ctx, instanceID, opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Started %s (state: %s)\n", instance.Name, instance.State)
	return nil
}

func handleStandby(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("instance name or ID required\nUsage: hypeman standby <instance>")
	}

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	instanceID, err := ResolveInstance(ctx, &client, args[0])
	if err != nil {
		return err
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	fmt.Fprintf(os.Stderr, "Putting %s into standby...\n", args[0])

	instance, err := client.Instances.Standby(ctx, instanceID, opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Standby %s (state: %s)\n", instance.Name, instance.State)
	return nil
}

func handleRestore(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("instance name or ID required\nUsage: hypeman restore <instance>")
	}

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	instanceID, err := ResolveInstance(ctx, &client, args[0])
	if err != nil {
		return err
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	fmt.Fprintf(os.Stderr, "Restoring %s from standby...\n", args[0])

	instance, err := client.Instances.Restore(ctx, instanceID, opts...)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Restored %s (state: %s)\n", instance.Name, instance.State)
	return nil
}
