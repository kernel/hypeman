package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kernel/hypeman-go"
	"github.com/kernel/hypeman-go/option"
	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v3"
)

var resourcesCmd = cli.Command{
	Name:  "resources",
	Usage: "Show server resource capacity and allocation status",
	Description: `Display current host resource capacity, allocation status, and per-instance breakdown.

Resources include CPU, memory, disk, network, and GPU (if available).
Oversubscription ratios are applied to calculate effective limits.

Examples:
  # Show all resources (default table format)
  hypeman resources

  # Show resources as JSON
  hypeman resources --format json

  # Show only GPU information
  hypeman resources --transform gpu`,
	Action:          handleResources,
	HideHelpCommand: true,
}

func handleResources(ctx context.Context, cmd *cli.Command) error {
	client := hypeman.NewClient(getDefaultRequestOptions(cmd)...)

	var opts []option.RequestOption
	if cmd.Root().Bool("debug") {
		opts = append(opts, debugMiddlewareOption)
	}

	var res []byte
	opts = append(opts, option.WithResponseBodyInto(&res))
	_, err := client.Resources.Get(ctx, opts...)
	if err != nil {
		return err
	}

	format := cmd.Root().String("format")
	transform := cmd.Root().String("transform")

	// If format is "auto", use our custom table format
	if format == "auto" || format == "" {
		return showResourcesTable(res)
	}

	// Otherwise use standard JSON display
	obj := gjson.ParseBytes(res)
	return ShowJSON(os.Stdout, "resources", obj, format, transform)
}

func showResourcesTable(data []byte) error {
	obj := gjson.ParseBytes(data)

	// Print resource summary table
	fmt.Println("RESOURCE   CAPACITY       EFFECTIVE      ALLOCATED      AVAILABLE      OVERSUB")
	fmt.Println(strings.Repeat("-", 75))

	printResourceRow("cpu", obj.Get("cpu"), "cores")
	printResourceRow("memory", obj.Get("memory"), "bytes")
	printResourceRow("disk", obj.Get("disk"), "bytes")
	printResourceRow("network", obj.Get("network"), "bps")

	// Print GPU information if available
	gpu := obj.Get("gpu")
	if gpu.Exists() && gpu.Type != gjson.Null {
		fmt.Println()
		printGPUInfo(gpu)
	}

	// Print disk breakdown if available
	diskBreakdown := obj.Get("disk_breakdown")
	if diskBreakdown.Exists() {
		fmt.Println()
		fmt.Println("DISK BREAKDOWN:")
		if v := diskBreakdown.Get("images_bytes").Int(); v > 0 {
			fmt.Printf("  Images:     %s\n", formatBytes(v))
		}
		if v := diskBreakdown.Get("volumes_bytes").Int(); v > 0 {
			fmt.Printf("  Volumes:    %s\n", formatBytes(v))
		}
		if v := diskBreakdown.Get("overlays_bytes").Int(); v > 0 {
			fmt.Printf("  Overlays:   %s\n", formatBytes(v))
		}
		if v := diskBreakdown.Get("oci_cache_bytes").Int(); v > 0 {
			fmt.Printf("  OCI Cache:  %s\n", formatBytes(v))
		}
	}

	// Print allocations if any
	allocations := obj.Get("allocations")
	if allocations.Exists() && allocations.IsArray() && len(allocations.Array()) > 0 {
		fmt.Println()
		fmt.Println("ALLOCATIONS:")
		fmt.Println("INSTANCE                      CPU    MEMORY     DISK       NET DOWN    NET UP")
		fmt.Println(strings.Repeat("-", 80))
		allocations.ForEach(func(key, value gjson.Result) bool {
			name := value.Get("instance_name").String()
			if len(name) > 28 {
				name = name[:25] + "..."
			}
			cpu := value.Get("cpu").Int()
			mem := formatBytes(value.Get("memory_bytes").Int())
			disk := formatBytes(value.Get("disk_bytes").Int())
			netDown := formatBps(value.Get("network_download_bps").Int())
			netUp := formatBps(value.Get("network_upload_bps").Int())
			fmt.Printf("%-28s  %3d    %-9s  %-9s  %-10s  %s\n", name, cpu, mem, disk, netDown, netUp)
			return true
		})
	}

	return nil
}

func printResourceRow(name string, res gjson.Result, unit string) {
	if !res.Exists() {
		return
	}

	capacity := res.Get("capacity").Int()
	effective := res.Get("effective_limit").Int()
	allocated := res.Get("allocated").Int()
	available := res.Get("available").Int()
	ratio := res.Get("oversub_ratio").Float()

	var capStr, effStr, allocStr, availStr string

	switch unit {
	case "bytes":
		capStr = formatBytes(capacity)
		effStr = formatBytes(effective)
		allocStr = formatBytes(allocated)
		availStr = formatBytes(available)
	case "bps":
		capStr = formatBps(capacity)
		effStr = formatBps(effective)
		allocStr = formatBps(allocated)
		availStr = formatBps(available)
	default:
		capStr = fmt.Sprintf("%d", capacity)
		effStr = fmt.Sprintf("%d", effective)
		allocStr = fmt.Sprintf("%d", allocated)
		availStr = fmt.Sprintf("%d", available)
	}

	ratioStr := fmt.Sprintf("%.1fx", ratio)
	if ratio == 1.0 {
		ratioStr = "1.0x"
	}

	fmt.Printf("%-10s %-14s %-14s %-14s %-14s %s\n", name, capStr, effStr, allocStr, availStr, ratioStr)
}

func printGPUInfo(gpu gjson.Result) {
	mode := gpu.Get("mode").String()
	totalSlots := gpu.Get("total_slots").Int()
	usedSlots := gpu.Get("used_slots").Int()

	fmt.Printf("GPU: %s mode (%d/%d slots used)\n", mode, usedSlots, totalSlots)

	if mode == "vgpu" {
		profiles := gpu.Get("profiles")
		if profiles.Exists() && profiles.IsArray() && len(profiles.Array()) > 0 {
			fmt.Println("PROFILE        VRAM       AVAILABLE")
			fmt.Println(strings.Repeat("-", 40))
			profiles.ForEach(func(key, value gjson.Result) bool {
				name := value.Get("name").String()
				framebufferMB := value.Get("framebuffer_mb").Int()
				available := value.Get("available").Int()
				vram := formatMB(framebufferMB)
				fmt.Printf("%-14s %-10s %d\n", name, vram, available)
				return true
			})
		}
	} else if mode == "passthrough" {
		devices := gpu.Get("devices")
		if devices.Exists() && devices.IsArray() && len(devices.Array()) > 0 {
			fmt.Println("DEVICE                         AVAILABLE")
			fmt.Println(strings.Repeat("-", 45))
			devices.ForEach(func(key, value gjson.Result) bool {
				name := value.Get("name").String()
				available := value.Get("available").Bool()
				availStr := "no"
				if available {
					availStr = "yes"
				}
				fmt.Printf("%-30s %s\n", name, availStr)
				return true
			})
		}
	}
}

func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)

	switch {
	case b >= TB:
		return fmt.Sprintf("%.1f TB", float64(b)/TB)
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/MB)
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/KB)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func formatMB(mb int64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// formatBps converts bytes per second (as returned by the API) to a human-readable
// bits per second string (Kbps, Mbps, Gbps). The API stores bandwidth in bytes/sec,
// but users specify and expect to see bandwidth in bits/sec (the standard unit for
// network bandwidth).
func formatBps(bytesPerSec int64) string {
	const (
		Kbps = 1000
		Mbps = Kbps * 1000
		Gbps = Mbps * 1000
	)

	// Convert bytes/sec to bits/sec (multiply by 8)
	bps := bytesPerSec * 8

	switch {
	case bps >= Gbps:
		return fmt.Sprintf("%.1f Gbps", float64(bps)/Gbps)
	case bps >= Mbps:
		return fmt.Sprintf("%.0f Mbps", float64(bps)/Mbps)
	case bps >= Kbps:
		return fmt.Sprintf("%.0f Kbps", float64(bps)/Kbps)
	default:
		return fmt.Sprintf("%d bps", bps)
	}
}
