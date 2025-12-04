// Example: Push a local Docker image to hypeman
//
// This example demonstrates how to push images to hypeman's registry using the SDK.
// It shows three approaches:
//  1. Push from local Docker daemon (most common for development)
//  2. Push from a remote registry (pull and push)
//  3. Push using PushFromURL helper (standalone scripts)
//
// Usage:
//
//	export HYPEMAN_API_KEY="your-jwt-token"
//	export HYPEMAN_BASE_URL="http://localhost:8080"
//	go run ./examples/push myapp:latest
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	hypeman "github.com/onkernel/hypeman-go"
	"github.com/onkernel/hypeman-go/lib"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: push <image> [target-name]")
		fmt.Fprintln(os.Stderr, "  image:       Local Docker image reference (e.g., myapp:latest)")
		fmt.Fprintln(os.Stderr, "  target-name: Optional name in hypeman (defaults to image)")
		os.Exit(1)
	}

	sourceImage := os.Args[1]
	targetName := sourceImage
	if len(os.Args) > 2 {
		targetName = os.Args[2]
	}

	ctx := context.Background()

	// Create a hypeman client (reads HYPEMAN_API_KEY and HYPEMAN_BASE_URL from env)
	client := hypeman.NewClient()

	// Extract push configuration from client options
	cfg, err := lib.ExtractPushConfig(client.Options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Pushing %s to hypeman as %s...\n", sourceImage, targetName)

	// Push from local Docker daemon
	err = lib.Push(ctx, cfg, sourceImage, targetName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Push failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Push successful!")
}

// Example: Push from a remote registry (pull from Docker Hub, push to hypeman)
func examplePushFromRemote(ctx context.Context, cfg lib.PushConfig) error {
	// Pull from Docker Hub
	ref, err := name.ParseReference("docker.io/library/alpine:latest")
	if err != nil {
		return err
	}

	img, err := remote.Image(ref)
	if err != nil {
		return err
	}

	// Push to hypeman
	return lib.PushImage(ctx, cfg, img, "alpine:latest")
}

// Example: Using PushFromURL for standalone scripts
func examplePushFromURL(ctx context.Context) error {
	baseURL := os.Getenv("HYPEMAN_BASE_URL")
	apiKey := os.Getenv("HYPEMAN_API_KEY")

	// Pull image from remote
	ref, err := name.ParseReference("docker.io/library/nginx:alpine")
	if err != nil {
		return err
	}

	img, err := remote.Image(ref)
	if err != nil {
		return err
	}

	// Push directly using URL (no client needed)
	return lib.PushFromURL(ctx, baseURL, apiKey, img, "nginx:alpine")
}
