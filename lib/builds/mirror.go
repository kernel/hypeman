package builds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/kernel/hypeman/lib/images"
)

// mirrorBaseImagesForBuild extracts base image references from the build's
// Dockerfile and mirrors each one to the local registry. BuildKit is configured
// with our registry as a mirror for docker.io, so pre-cached images will be
// served locally without pulling from Docker Hub.
//
// Configured source repositories are required. Other images retain best-effort prefetching.
func (m *manager) mirrorBaseImagesForBuild(ctx context.Context, id string, req CreateBuildRequest) error {
	// Match the builder: an archived Dockerfile takes precedence over the inline one.
	dockerfileContent, err := ExtractDockerfileFromTarball(m.paths.BuildSourceDir(id) + "/source.tar.gz")
	if errors.Is(err, errDockerfileNotFound) || errors.Is(err, os.ErrNotExist) {
		dockerfileContent, err = req.Dockerfile, nil
	}
	if err != nil {
		if len(m.config.BaseImageSources) > 0 {
			return fmt.Errorf("read Dockerfile for required base images: %w", err)
		}
		m.logger.Warn("could not extract Dockerfile from tarball for mirroring", "id", id, "error", err)
		return nil
	}

	// Parse FROM references
	refs := ParseDockerfileFROMs(dockerfileContent)
	if len(refs) == 0 {
		return nil
	}

	requests := make([]images.MirrorRequest, 0, len(refs))
	required := false
	for _, ref := range refs {
		parsed, err := name.ParseReference(ref)
		if err != nil {
			return fmt.Errorf("parse base image: %w", err)
		}
		source := m.config.BaseImageSources[parsed.Context().Name()]
		required = required || source != ""
		requests = append(requests, images.MirrorRequest{SourceImage: ref, SourceRepository: source})
	}

	m.logger.Info("mirroring base images to local registry", "id", id, "images", refs)

	// Generate a scoped registry token that grants push access to the base
	// image repos. The local registry requires JWT auth for all operations;
	// go-containerregistry uses this via the Docker token auth flow (Basic
	// auth username = JWT → /v2/token validates and returns bearer token).
	// Build repo permissions. The Docker token scope uses the repo name without
	// the tag (e.g. "onkernel/nodejs22-base", not "onkernel/nodejs22-base:0.1.1").
	seen := make(map[string]bool)
	var repoPerms []RepoPermission
	for _, ref := range refs {
		repo := ref
		if idx := strings.LastIndex(repo, "@"); idx != -1 {
			repo = repo[:idx]
		}
		if idx := strings.LastIndex(repo, ":"); idx > 0 {
			repo = repo[:idx]
		}
		if !seen[repo] {
			seen[repo] = true
			repoPerms = append(repoPerms, RepoPermission{Repo: repo, Scope: "push"})
		}
	}
	registryToken, err := m.tokenGenerator.GenerateToken(id, repoPerms, 10*time.Minute)
	if err != nil {
		if required {
			return fmt.Errorf("authorize required base images: %w", err)
		}
		m.logger.Warn("failed to generate registry token for mirroring",
			"id", id, "error", err)
		return nil
	}
	// go-containerregistry's basicTransport only sends Basic auth when BOTH
	// Username and Password are non-empty. The password value doesn't matter —
	// our token handler extracts the JWT from the username field only.
	authConfig := &authn.AuthConfig{Username: registryToken, Password: "x"}

	for _, request := range requests {
		ref := request.SourceImage
		result, err := images.MirrorBaseImage(ctx, m.config.RegistryURL, request, authConfig)
		if err != nil {
			if request.SourceRepository != "" {
				return fmt.Errorf("prefetch required base image %s: %w", ref, err)
			}
			m.logger.Warn("failed to mirror base image",
				"id", id, "image", ref, "error", err)
			continue
		}
		m.logger.Info("mirrored base image",
			"id", id, "image", ref, "local_ref", result.LocalRef, "digest", result.Digest)
	}

	return nil
}

func normalizeBaseImageSources(sources map[string]string) (map[string]string, error) {
	normalized := make(map[string]string, len(sources))
	for source, target := range sources {
		src, err := name.NewRepository(source)
		if err != nil {
			return nil, fmt.Errorf("invalid base image repository %q: %w", source, err)
		}
		dst, err := name.NewRepository(target)
		if err != nil {
			return nil, fmt.Errorf("invalid base image source %q: %w", target, err)
		}
		if previous, exists := normalized[src.Name()]; exists && previous != dst.Name() {
			return nil, fmt.Errorf("conflicting sources for base image repository %q", source)
		}
		normalized[src.Name()] = dst.Name()
	}
	return normalized, nil
}
