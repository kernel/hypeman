package images

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// OCIOption configures image fetching.
type OCIOption func(*ociClient) error

// WithDockerHubMirror routes host-authenticated Docker Hub pulls through a registry prefix.
// Caller-supplied credentials always use the original registry.
func WithDockerHubMirror(prefix string) OCIOption {
	return func(client *ociClient) error {
		if prefix != "" {
			if _, err := dockerHubSource(name.MustParseReference("alpine"), prefix, nil); err != nil {
				return err
			}
		}
		client.dockerHubMirror = prefix
		return nil
	}
}

func dockerHubSource(ref name.Reference, prefix string, credentials *authn.AuthConfig) (name.Reference, error) {
	if prefix == "" || credentialsPresent(credentials) || ref.Context().RegistryStr() != name.DefaultRegistry {
		return ref, nil
	}
	if strings.Contains(prefix, "://") || strings.ContainsAny(prefix, "@ \t\n") {
		return nil, fmt.Errorf("Docker Hub mirror must be a registry/repository prefix")
	}
	repo, err := name.NewRepository(strings.TrimSuffix(prefix, "/") + "/" + ref.Context().RepositoryStr())
	if err != nil {
		return nil, fmt.Errorf("parse Docker Hub mirror: %w", err)
	}
	if repo.RegistryStr() == name.DefaultRegistry {
		return nil, fmt.Errorf("Docker Hub mirror must name a different registry")
	}
	if digest, ok := ref.(name.Digest); ok {
		return repo.Digest(digest.DigestStr()), nil
	}
	return repo.Tag(ref.Identifier()), nil
}
