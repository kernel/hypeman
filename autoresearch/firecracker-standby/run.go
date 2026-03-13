//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kernel/hypeman/autoresearch/firecracker-standby/bench"
)

func main() {
	var scenario string
	var workspace string
	var results string
	var budget time.Duration
	var timeout time.Duration
	var status string
	var description string

	flag.StringVar(&scenario, "scenario", bench.DefaultScenario, "benchmark scenario to run")
	flag.StringVar(&workspace, "workspace", "", "override workspace root (defaults under tmp/)")
	flag.StringVar(&results, "results", "", "results TSV path (defaults inside workspace)")
	flag.DurationVar(&budget, "budget", 180*time.Second, "fixed benchmark wall-clock budget")
	flag.DurationVar(&timeout, "running-timeout", 30*time.Second, "timeout for waiting on running state after restore")
	flag.StringVar(&status, "status", "", "results row status (for example baseline, keep, discard, crash)")
	flag.StringVar(&description, "description", "", "short results row description")
	flag.Parse()

	summary, err := bench.Run(context.Background(), bench.RunOptions{
		Scenario:       scenario,
		WorkspaceRoot:  workspace,
		Budget:         budget,
		RunningTimeout: timeout,
		ResultsPath:    results,
		Status:         status,
		Description:    description,
	})
	if summary != nil {
		fmt.Println("---")
		fmt.Printf("scenario:                %s\n", summary.Scenario)
		fmt.Printf("budget_seconds:          %.1f\n", summary.Budget.Seconds())
		fmt.Printf("duration_seconds:        %.1f\n", summary.Duration.Seconds())
		fmt.Printf("cycles:                  %d\n", summary.Cycles)
		fmt.Printf("failures:                %d\n", summary.Failures)
		fmt.Printf("score_ms:                %.1f\n", summary.ScoreMs)
		fmt.Printf("standby_p50_ms:          %.1f\n", summary.StandbyP50Ms)
		fmt.Printf("restore_api_p50_ms:      %.1f\n", summary.RestoreAPIP50Ms)
		fmt.Printf("restore_running_p50_ms:  %.1f\n", summary.RestoreRunningP50Ms)
		fmt.Printf("restore_running_p95_ms:  %.1f\n", summary.RestoreRunningP95Ms)
		fmt.Printf("branch:                  %s\n", summary.Branch)
		fmt.Printf("commit:                  %s\n", summary.Commit)
		fmt.Printf("results_path:            %s\n", summary.ResultsPath)

		data, jsonErr := json.Marshal(summary)
		if jsonErr == nil {
			fmt.Printf("summary_json:            %s\n", data)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "run failed: %v\n", err)
		os.Exit(1)
	}
}
