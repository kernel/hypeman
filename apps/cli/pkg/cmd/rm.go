package cmd

import (
	"context"
	"fmt"

	"github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/option"
	"github.com/urfave/cli/v3"
)

var rmCmd = cli.Command{
	Name:      "rm",
	Usage:     "Remove one or more instances",
	ArgsUsage: "[instance...]",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "force",
			Aliases: []string{"f"},
			Usage:   "Force removal of running instances",
		},
		&cli.BoolFlag{
			Name:  "all",
			Usage: "Remove all instances (stopped only, unless --force)",
		},
	},
	Action:          handleRm,
	HideHelpCommand: true,
}

func handleRm(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	force := cmd.Bool("force")
	all := cmd.Bool("all")

	if !all && len(args) < 1 {
		return fmt.Errorf("instance ID required\nUsage: hypeman rm [flags] <instance> [instance...]\n       hypeman rm --all [--force]")
	}

	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	// If --all, get all instance IDs
	var identifiers []string
	if all {
		instances, err := client.Instances.List(ctx)
		if err != nil {
			return fmt.Errorf("failed to list instances: %w", err)
		}
		for _, inst := range *instances {
			identifiers = append(identifiers, inst.ID)
		}
		if len(identifiers) == 0 {
			fmt.Println("No instances to remove")
			return nil
		}
	} else {
		identifiers = args
	}

	var lastErr error
	for _, identifier := range identifiers {
		// Resolve instance by ID, partial ID, or name (skip if --all since we have full IDs)
		var instanceID string
		var err error
		if all {
			instanceID = identifier
		} else {
			instanceID, err = ResolveInstance(ctx, &client, identifier)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				lastErr = err
				continue
			}
		}

		// Check instance state if not forcing
		if !force {
			inst, err := client.Instances.Get(
				ctx,
				instanceID,
				option.WithMiddleware(debugMiddleware(cmd.Root().Bool("debug"))),
			)
			if err != nil {
				fmt.Printf("Error: failed to get instance %s: %v\n", instanceID, err)
				lastErr = err
				continue
			}

			if inst.State == "Running" {
				if all {
					// Silently skip running instances when using --all without --force
					continue
				}
				fmt.Printf("Error: cannot remove running instance %s. Stop it first or use --force\n", instanceID)
				lastErr = fmt.Errorf("instance is running")
				continue
			}
		}

		// Delete the instance
		err = client.Instances.Delete(
			ctx,
			instanceID,
			option.WithMiddleware(debugMiddleware(cmd.Root().Bool("debug"))),
		)
		if err != nil {
			fmt.Printf("Error: failed to remove instance %s: %v\n", instanceID, err)
			lastErr = err
			continue
		}

		fmt.Println(instanceID)
	}

	return lastErr
}

