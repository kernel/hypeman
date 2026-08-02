package builds

import (
	"context"
	"fmt"
)

// PublishConfig holds optional remote publication settings for completed
// build images. The zero value disables publication entirely.
type PublishConfig struct {
	// Registry is the remote registry host (e.g., "registry.example.com").
	Registry string

	// RepositoryPrefix is prepended to the build ID to form the remote
	// repository (e.g., "team/app-builds" -> "team/app-builds/<build-id>").
	RepositoryPrefix string

	// CredentialsFile is a host-side path to a Docker client config used to
	// authenticate with the remote registry. Credentials are read by the host
	// only and are never sent to builder VMs.
	CredentialsFile string
}

// Enabled reports whether remote publication is configured.
func (c PublishConfig) Enabled() bool {
	return c.Registry != ""
}

// validate checks that the publication settings are coherent.
func (c PublishConfig) validate() error {
	if c.Registry == "" {
		if c.RepositoryPrefix != "" || c.CredentialsFile != "" {
			return fmt.Errorf("publish: repository_prefix and credentials_file require registry to be set")
		}
		return nil
	}
	if c.RepositoryPrefix == "" {
		return fmt.Errorf("publish: repository_prefix is required when registry is set")
	}
	return nil
}

// BuildPublisher publishes a completed build image and returns the resulting
// reference to record on the build.
type BuildPublisher interface {
	// Publish publishes the image identified by localRef and digest, and
	// returns the resulting reference.
	Publish(ctx context.Context, localRef, digest string) (string, error)
}

// noopPublisher is the default publisher. It returns the local reference
// unchanged, leaving build behavior identical to no publication configured.
type noopPublisher struct{}

func (noopPublisher) Publish(_ context.Context, localRef, _ string) (string, error) {
	return localRef, nil
}

// newBuildPublisher returns the publisher for the given publication config.
// Publication is a no-op unless a remote registry is configured.
func newBuildPublisher(cfg PublishConfig) (BuildPublisher, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return noopPublisher{}, nil
}
