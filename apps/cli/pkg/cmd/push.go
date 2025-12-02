package cmd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/urfave/cli/v3"
)

var pushCmd = cli.Command{
	Name:            "push",
	Usage:           "Push a local Docker image to hypeman",
	ArgsUsage:       "<image> [target-name]",
	Action:          handlePush,
	HideHelpCommand: true,
}

func handlePush(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("image reference required\nUsage: hypeman push <image>")
	}

	sourceImage := args[0]
	targetName := sourceImage
	if len(args) > 1 {
		targetName = args[1]
	}

	baseURL := cmd.String("base-url")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}

	registryHost := parsedURL.Host

	fmt.Fprintf(os.Stderr, "Loading image %s from Docker...\n", sourceImage)

	srcRef, err := name.ParseReference(sourceImage)
	if err != nil {
		return fmt.Errorf("invalid source image: %w", err)
	}

	img, err := daemon.Image(srcRef)
	if err != nil {
		return fmt.Errorf("load image: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return fmt.Errorf("get image digest: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Digest: %s\n", digest.String())

	// Strip any tag from targetName and use digest reference instead
	// This ensures the server triggers image conversion
	targetBase := strings.TrimPrefix(targetName, "/")
	if idx := strings.LastIndex(targetBase, ":"); idx != -1 && !strings.Contains(targetBase[idx:], "/") {
		targetBase = targetBase[:idx]
	}
	if idx := strings.LastIndex(targetBase, "@"); idx != -1 {
		targetBase = targetBase[:idx]
	}

	targetRef := registryHost + "/" + targetBase + "@" + digest.String()
	fmt.Fprintf(os.Stderr, "Pushing to %s...\n", targetRef)

	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	if err != nil {
		return fmt.Errorf("invalid target: %w", err)
	}

	auth := &hypemanAuth{}

	err = remote.Write(dstRef, img,
		remote.WithContext(ctx),
		remote.WithAuth(auth),
	)
	if err != nil {
		return fmt.Errorf("push failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Pushed %s\n", targetRef)
	return nil
}

type hypemanAuth struct{}

func (a *hypemanAuth) Authorization() (*authn.AuthConfig, error) {
	token := os.Getenv("HYPEMAN_BEARER_TOKEN")
	if token == "" {
		token = os.Getenv("HYPEMAN_API_KEY")
	}
	if token == "" {
		return &authn.AuthConfig{}, nil
	}
	return &authn.AuthConfig{RegistryToken: token}, nil
}
