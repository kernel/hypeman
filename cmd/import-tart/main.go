//go:build darwin && arm64

// import-tart publishes a stopped Tart macOS VM to hypeman's registry as a machine
// image. A stacked VM built on an image already in the registry passes that image
// as --base, so only its overlay is uploaded.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v4"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/network"
)

func main() {
	vm := flag.String("vm", "", "stopped Tart VM directory, e.g. ~/.tart/vms/NAME")
	registry := flag.String("registry", "", "hypeman registry URL, e.g. https://hypeman.example or http://localhost:4973")
	name := flag.String("name", "", "image name; the image is published as builds/NAME")
	base := flag.String("base", "", "digest-pinned registry image the VM's stacked disk is built on")
	guestAgent := flag.Bool("guest-agent", false, "the image runs hypeman's guest agent")
	timeout := flag.Duration("timeout", 2*time.Hour, "publication timeout")
	flag.Parse()
	secret := os.Getenv("HYPEMAN_JWT_SECRET")
	if *vm == "" || *registry == "" || *name == "" || secret == "" {
		fmt.Fprintln(os.Stderr, "--vm, --registry, --name and HYPEMAN_JWT_SECRET are required")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()
	publication, err := run(ctx, *vm, *registry, *name, *base, *guestAgent, secret)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(publication); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, vm, registry, name, baseRef string, guestAgent bool, secret string) (builds.MachinePublication, error) {
	storage, err := builds.ReadTartVM(vm, guestAgent)
	if err != nil {
		return builds.MachinePublication{}, err
	}
	for _, path := range append([]string{storage.Overlay, storage.Aux}, storage.Layers...) {
		if err := images.RequireClosedFile(ctx, path); err != nil {
			return builds.MachinePublication{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	var base *builds.MachineBase
	if baseRef != "" {
		if base, err = builds.ParseMachineBase(registry, baseRef); err != nil {
			return builds.MachinePublication{}, err
		}
	}
	// Tart keeps its source's identity, so each image gets its own.
	id, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return builds.MachinePublication{}, err
	}
	storage.Platform.MachineIdentifier = id.DataRepresentation()
	if storage.Platform.MAC, err = network.NewUniqueMAC(nil); err != nil {
		return builds.MachinePublication{}, err
	}
	publisher := &builds.StreamingMachinePublisher{RegistryURL: registry, Tokens: builds.NewRegistryTokenGenerator(secret)}
	return publisher.Publish(ctx, name, storage, base)
}
