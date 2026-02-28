package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v3"
)

var deviceCmd = cli.Command{
	Name:  "device",
	Usage: "Manage PCI/GPU devices for passthrough",
	Description: `Manage PCI devices for passthrough to virtual machines.

This command allows you to discover available passthrough-capable devices,
register them for use with instances, and manage registered devices.

Examples:
  # Discover available devices on the host
  hypeman device available

  # Register a GPU for passthrough
  hypeman device register --pci-address 0000:a2:00.0 --name my-gpu

  # List registered devices
  hypeman device list

  # Delete a registered device
  hypeman device delete my-gpu`,
	Commands: []*cli.Command{
		&deviceAvailableCmd,
		&deviceRegisterCmd,
		&deviceListCmd,
		&deviceGetCmd,
		&deviceDeleteCmd,
	},
	HideHelpCommand: true,
}

var deviceAvailableCmd = cli.Command{
	Name:  "available",
	Usage: "Discover passthrough-capable devices on host",
	Description: `List all PCI devices on the host that are capable of passthrough.

Shows devices with their PCI address, vendor/device info, IOMMU group,
and current driver binding.`,
	Action:          handleDeviceAvailable,
	HideHelpCommand: true,
}

var deviceRegisterCmd = cli.Command{
	Name:      "register",
	Usage:     "Register a device for passthrough",
	ArgsUsage: "[pci-address]",
	Description: `Register a PCI device for use with VM passthrough.

The device must be in an IOMMU group that supports passthrough.
Once registered, the device can be attached to instances using
the --device flag with 'hypeman run'.

Examples:
  # Register by PCI address
  hypeman device register 0000:a2:00.0

  # Register with a custom name
  hypeman device register --pci-address 0000:a2:00.0 --name my-gpu`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "pci-address",
			Usage: "PCI address of the device (e.g., 0000:a2:00.0)",
		},
		&cli.StringFlag{
			Name:  "name",
			Usage: "Optional name for the device (auto-generated if not provided)",
		},
	},
	Action:          handleDeviceRegister,
	HideHelpCommand: true,
}

var deviceListCmd = cli.Command{
	Name:            "list",
	Usage:           "List registered devices",
	Action:          handleDeviceList,
	HideHelpCommand: true,
}

var deviceGetCmd = cli.Command{
	Name:      "get",
	Usage:     "Get device details",
	ArgsUsage: "<device-id-or-name>",
	Action:    handleDeviceGet,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "id",
			Usage: "Device ID or name",
		},
	},
	HideHelpCommand: true,
}

var deviceDeleteCmd = cli.Command{
	Name:      "delete",
	Aliases:   []string{"rm", "unregister"},
	Usage:     "Unregister a device",
	ArgsUsage: "<device-id-or-name>",
	Action:    handleDeviceDelete,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "id",
			Usage: "Device ID or name",
		},
	},
	HideHelpCommand: true,
}

func handleDeviceAvailable(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	var res []byte
	opts = append(opts, option.WithResponseBodyInto(&res))
	_, err := client.Devices.ListAvailable(ctx, opts...)
	if err != nil {
		return err
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	// If format is "auto", use our custom table format
	if format == "auto" || format == "" {
		return showAvailableDevicesTable(res)
	}

	obj := gjson.ParseBytes(res)
	return ShowJSON(os.Stdout, "devices available", obj, format, transform)
}

func showAvailableDevicesTable(data []byte) error {
	devices := gjson.ParseBytes(data)

	if !devices.IsArray() || len(devices.Array()) == 0 {
		fmt.Println("No passthrough-capable devices found.")
		return nil
	}

	table := NewTableWriter(os.Stdout, "PCI ADDRESS", "VENDOR", "DEVICE", "IOMMU", "DRIVER")
	table.TruncOrder = []int{2, 1} // DEVICE first, then VENDOR

	devices.ForEach(func(key, value gjson.Result) bool {
		pciAddr := value.Get("pci_address").String()
		vendorID := value.Get("vendor_id").String()
		deviceID := value.Get("device_id").String()
		vendorName := value.Get("vendor_name").String()
		deviceName := value.Get("device_name").String()
		iommuGroup := fmt.Sprintf("%d", value.Get("iommu_group").Int())
		driver := value.Get("current_driver").String()

		vendor := vendorName
		if vendor == "" {
			vendor = vendorID
		}

		device := deviceName
		if device == "" {
			device = deviceID
		}

		if driver == "" {
			driver = "-"
		}

		table.AddRow(pciAddr, vendor, device, iommuGroup, driver)
		return true
	})

	table.Render()
	return nil
}

func handleDeviceRegister(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	// Get PCI address from flag or first argument
	pciAddress := cmd.String("pci-address")
	args := cmd.Args().Slice()
	if pciAddress == "" && len(args) > 0 {
		pciAddress = args[0]
	}

	if pciAddress == "" {
		return fmt.Errorf("PCI address required\nUsage: hypeman device register [--pci-address] <pci-address> [--name <name>]")
	}

	params := hypeman.DeviceNewParams{
		PciAddress: pciAddress,
	}

	if name := cmd.String("name"); name != "" {
		params.Name = hypeman.Opt(name)
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	var res []byte
	opts = append(opts, option.WithResponseBodyInto(&res))
	_, err := client.Devices.New(ctx, params, opts...)
	if err != nil {
		return err
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	if format == "auto" || format == "" {
		device := gjson.ParseBytes(res)
		fmt.Printf("Registered device %s (%s)\n", device.Get("name").String(), device.Get("id").String())
		return nil
	}

	obj := gjson.ParseBytes(res)
	return ShowJSON(os.Stdout, "device register", obj, format, transform)
}

func handleDeviceList(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	var res []byte
	opts = append(opts, option.WithResponseBodyInto(&res))
	_, err := client.Devices.List(ctx, opts...)
	if err != nil {
		return err
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	if format == "auto" || format == "" {
		return showDeviceListTable(res)
	}

	obj := gjson.ParseBytes(res)
	return ShowJSON(os.Stdout, "devices list", obj, format, transform)
}

func showDeviceListTable(data []byte) error {
	devices := gjson.ParseBytes(data)

	if !devices.IsArray() || len(devices.Array()) == 0 {
		fmt.Println("No registered devices.")
		return nil
	}

	table := NewTableWriter(os.Stdout, "ID", "NAME", "TYPE", "PCI ADDRESS", "VFIO", "ATTACHED TO")
	table.TruncOrder = []int{0, 1, 5} // ID first, then NAME, ATTACHED TO

	devices.ForEach(func(key, value gjson.Result) bool {
		id := value.Get("id").String()
		name := value.Get("name").String()
		deviceType := value.Get("type").String()
		pciAddr := value.Get("pci_address").String()

		vfio := "no"
		if value.Get("bound_to_vfio").Bool() {
			vfio = "yes"
		}

		attachedTo := value.Get("attached_to").String()
		if attachedTo == "" {
			attachedTo = "-"
		}

		table.AddRow(id, name, deviceType, pciAddr, vfio, attachedTo)
		return true
	})

	table.Render()
	return nil
}

func handleDeviceGet(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	// Get device ID from flag or first argument
	id := cmd.String("id")
	args := cmd.Args().Slice()
	if id == "" && len(args) > 0 {
		id = args[0]
	}

	if id == "" {
		return fmt.Errorf("device ID or name required\nUsage: hypeman device get <device-id-or-name>")
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	var res []byte
	opts = append(opts, option.WithResponseBodyInto(&res))
	_, err := client.Devices.Get(ctx, id, opts...)
	if err != nil {
		return err
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	obj := gjson.ParseBytes(res)
	return ShowJSON(os.Stdout, "device get", obj, format, transform)
}

func handleDeviceDelete(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	// Get device ID from flag or first argument
	id := cmd.String("id")
	args := cmd.Args().Slice()
	if id == "" && len(args) > 0 {
		id = args[0]
	}

	if id == "" {
		return fmt.Errorf("device ID or name required\nUsage: hypeman device delete <device-id-or-name>")
	}

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	err := client.Devices.Delete(ctx, id, opts...)
	if err != nil {
		return err
	}

	fmt.Printf("Deleted device %s\n", id)
	return nil
}
