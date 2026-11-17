//go:build darwin && arm64

// import-tart publishes a stopped Tart macOS VM to hypeman's registry as a machine
// image. A stacked VM built on an image already in the registry passes that image
// as --base, so only its overlay is uploaded. With --oci-layout it writes the image
// to a local OCI image layout instead, to push to another registry with its own
// tooling and credentials.
//
// It authenticates with HYPEMAN_REGISTRY_TOKEN, a token from --mint-token that can
// publish only that image over that base, or with the registry's HYPEMAN_JWT_SECRET.
// --mint-token needs the secret and prints the token; CI then holds the token only.
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
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/machineimage"
	"github.com/kernel/hypeman/lib/registrytoken"
)

func main() {
	vm := flag.String("vm", "", "stopped Tart VM directory, e.g. ~/.tart/vms/NAME")
	registry := flag.String("registry", "", "hypeman registry URL, e.g. https://hypeman.example or http://localhost:4973")
	name := flag.String("name", "", "image name; the image is published as builds/NAME")
	baseRef := flag.String("base", "", "digest-pinned registry image the VM's stacked disk is built on")
	guestAgent := flag.Bool("guest-agent", false, "the image runs hypeman's guest agent")
	timeout := flag.Duration("timeout", 2*time.Hour, "publication timeout")
	mint := flag.Bool("mint-token", false, "print a registry token that can publish --name over --base, instead of publishing")
	tokenTTL := flag.Duration("token-ttl", 30*24*time.Hour, "lifetime of a token from --mint-token")
	ociLayout := flag.String("oci-layout", "", "write the image to a new OCI image layout at this directory instead of publishing it")
	flag.Parse()
	if *ociLayout != "" {
		if *vm == "" || *registry != "" || *baseRef != "" || *mint {
			fmt.Fprintln(os.Stderr, "--oci-layout needs --vm and takes no --registry, --base or --mint-token")
			os.Exit(2)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		storage, err := readClosedVM(ctx, *vm, *guestAgent)
		if err == nil {
			var publication machineimage.Publication
			if publication, err = machineimage.WriteLayout(ctx, *ociLayout, storage, newMachineIdentifier); err == nil {
				err = json.NewEncoder(os.Stdout).Encode(publication)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	token, secret := os.Getenv("HYPEMAN_REGISTRY_TOKEN"), os.Getenv("HYPEMAN_JWT_SECRET")
	if *registry == "" || *name == "" || (*mint && secret == "") || (!*mint && (*vm == "" || token == "" && secret == "")) {
		fmt.Fprintln(os.Stderr, "--registry and --name are required, with --vm and HYPEMAN_REGISTRY_TOKEN or HYPEMAN_JWT_SECRET to publish, or --mint-token and HYPEMAN_JWT_SECRET")
		os.Exit(2)
	}
	var base *machineimage.Base
	if *baseRef != "" {
		var err error
		if base, err = machineimage.ParseBase(*registry, *baseRef); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *mint {
		minted, err := machineimage.MintToken(registrytoken.NewGenerator(secret), *name, base, *tokenTTL)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(minted)
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()
	publisher := &machineimage.Publisher{RegistryURL: *registry, Token: token, NewMachineIdentifier: newMachineIdentifier}
	if token == "" {
		publisher.Tokens = registrytoken.NewGenerator(secret)
	}
	publication, err := publish(ctx, publisher, *vm, *name, base, *guestAgent)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(publication); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func publish(ctx context.Context, publisher *machineimage.Publisher, vm, name string, base *machineimage.Base, guestAgent bool) (machineimage.Publication, error) {
	storage, err := readClosedVM(ctx, vm, guestAgent)
	if err != nil {
		return machineimage.Publication{}, err
	}
	return publisher.Publish(ctx, name, storage, base)
}

// readClosedVM reads the VM's storage and checks that no process has its files open.
func readClosedVM(ctx context.Context, vm string, guestAgent bool) (machineimage.Storage, error) {
	storage, err := machineimage.ReadTartVM(vm, guestAgent)
	if err != nil {
		return machineimage.Storage{}, err
	}
	for _, path := range append([]string{storage.Overlay, storage.Aux}, storage.Layers...) {
		if err := images.RequireClosedFile(ctx, path); err != nil {
			return machineimage.Storage{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	return storage, nil
}

func newMachineIdentifier() ([]byte, error) {
	id, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return nil, err
	}
	return id.DataRepresentation(), nil
}
