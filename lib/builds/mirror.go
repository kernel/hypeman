package builds

import (
	"context"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/kernel/hypeman/lib/images"
)

// mirrorBaseImagesForBuild extracts base image references from the build's
// Dockerfile and mirrors each one to the local registry. BuildKit is configured
// with our registry as a mirror for docker.io, so pre-cached images will be
// served locally without pulling from Docker Hub.
//
// Individual mirror failures are logged but do not fail the build (graceful
// degradation — BuildKit will pull from Docker Hub as before).
func (m *manager) mirrorBaseImagesForBuild(ctx context.Context, id string, req CreateBuildRequest) error {
	// Get Dockerfile content: prefer inline Dockerfile, fall back to tarball
	var dockerfileContent string
	if req.Dockerfile != "" {
		dockerfileContent = req.Dockerfile
	} else {
		tarballPath := m.paths.BuildSourceDir(id) + "/source.tar.gz"
		content, err := ExtractDockerfileFromTarball(tarballPath)
		if err != nil {
			m.logger.Warn("could not extract Dockerfile from tarball for mirroring",
				"id", id, "error", err)
			return nil
		}
		dockerfileContent = content
	}

	// Parse FROM references
	refs := ParseDockerfileFROMs(dockerfileContent)
	if len(refs) == 0 {
		return nil
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
		m.logger.Warn("failed to generate registry token for mirroring",
			"id", id, "error", err)
		return nil
	}
	// go-containerregistry's basicTransport only sends Basic auth when BOTH
	// Username and Password are non-empty. The password value doesn't matter —
	// our token handler extracts the JWT from the username field only.
	authConfig := &authn.AuthConfig{Username: registryToken, Password: "x"}

	for _, ref := range refs {
		result, err := images.MirrorBaseImage(ctx, m.config.RegistryURL, images.MirrorRequest{
			SourceImage: ref,
		}, authConfig)
		if err != nil {
			m.logger.Warn("failed to mirror base image",
				"id", id, "image", ref, "error", err)
			continue
		}
		m.logger.Info("mirrored base image",
			"id", id, "image", ref, "local_ref", result.LocalRef, "digest", result.Digest)
	}

	return nil
}
