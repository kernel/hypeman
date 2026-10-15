//go:build darwin && arm64

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
)

func main() {
	data := flag.String("data-dir", "", "Hypeman data directory")
	source := flag.String("source", "", "stopped macvm bundle")
	name := flag.String("name", "", "local image name (e.g. localhost/macos:spike)")
	flag.Parse()
	if *data == "" || *source == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "--data-dir, --source and --name are required")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	img, err := images.ImportMacOSImage(ctx, paths.New(*data), *name, *source)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	json.NewEncoder(os.Stdout).Encode(img)
}
