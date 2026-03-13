//go:build ignore

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kernel/hypeman/autoresearch/firecracker-standby/bench"
)

func main() {
	var scenario string
	var workspace string

	flag.StringVar(&scenario, "scenario", bench.DefaultScenario, "benchmark scenario to prepare")
	flag.StringVar(&workspace, "workspace", "", "override workspace root (defaults under tmp/)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	manifest, err := bench.Prepare(ctx, bench.PrepareOptions{
		Scenario:      scenario,
		WorkspaceRoot: workspace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "prepare failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("---")
	fmt.Printf("scenario:       %s\n", manifest.Scenario)
	fmt.Printf("workspace_root: %s\n", manifest.WorkspaceRoot)
	fmt.Printf("data_dir:       %s\n", manifest.DataDir)
	fmt.Printf("instance_id:    %s\n", manifest.InstanceID)
	fmt.Printf("instance_name:  %s\n", manifest.InstanceName)
	fmt.Printf("image_ref:      %s\n", manifest.ImageRef)
	fmt.Printf("branch:         %s\n", manifest.Branch)
	fmt.Printf("commit:         %s\n", manifest.Commit)
	fmt.Printf("prepared_at:    %s\n", manifest.PreparedAt.Format(time.RFC3339))
}
