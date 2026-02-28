package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	"github.com/urfave/cli/v3"
)

var psCmd = cli.Command{
	Name:  "ps",
	Usage: "List instances",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "all",
			Aliases: []string{"a"},
			Usage:   "Show all instances (default: running only)",
		},
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Only display instance IDs",
		},
	},
	Action:          handlePs,
	HideHelpCommand: true,
}

func handlePs(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	instances, err := client.Instances.List(
		ctx,
		opts...,
	)
	if err != nil {
		return err
	}

	showAll := cmd.Bool("all")
	quietMode := cmd.Bool("quiet")

	// Filter instances
	var filtered []hypeman.Instance
	for _, inst := range *instances {
		if showAll || inst.State == "Running" {
			filtered = append(filtered, inst)
		}
	}

	// Quiet mode - just IDs
	if quietMode {
		for _, inst := range filtered {
			fmt.Println(inst.ID)
		}
		return nil
	}

	// Table output
	if len(filtered) == 0 {
		if !showAll {
			fmt.Fprintln(os.Stderr, "No running instances. Use -a to show all.")
		}
		return nil
	}

	table := NewTableWriter(os.Stdout, "INSTANCE ID", "NAME", "IMAGE", "STATE", "GPU", "HV", "CREATED")
	table.TruncOrder = []int{2, 4, 6, 1} // IMAGE first, then GPU, CREATED, NAME
	for _, inst := range filtered {
		table.AddRow(
			TruncateID(inst.ID),
			inst.Name,
			inst.Image,
			string(inst.State),
			formatGPU(inst.GPU),
			formatHypervisor(inst.Hypervisor),
			FormatTimeAgo(inst.CreatedAt),
		)
	}
	table.Render()

	return nil
}

// formatGPU returns a short representation of GPU configuration
func formatGPU(gpu hypeman.InstanceGPU) string {
	// Check if GPU profile is set
	if gpu.Profile != "" {
		return gpu.Profile
	}
	// Check if mdev UUID is set (indicates vGPU without profile name shown)
	if gpu.MdevUuid != "" {
		return "vgpu"
	}
	return "-"
}

// formatHypervisor returns a short abbreviation for the hypervisor
func formatHypervisor(hv hypeman.InstanceHypervisor) string {
	switch hv {
	case hypeman.InstanceHypervisorCloudHypervisor:
		return "ch"
	case hypeman.InstanceHypervisorQemu:
		return "qemu"
	case hypeman.InstanceHypervisorVz:
		return "vz"
	default:
		if hv == "" {
			return "ch" // default
		}
		return string(hv)
	}
}
