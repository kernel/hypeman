// Package builds implements a secure build system that runs rootless BuildKit
// inside ephemeral Cloud Hypervisor microVMs for multi-tenant isolation.
package builds

import "time"

// Build status constants
const (
	StatusQueued    = "queued"
	StatusBuilding  = "building"
	StatusPushing   = "pushing"
	StatusReady     = "ready"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Build represents a source-to-image build job
type Build struct {
	ID            string           `json:"id"`
	Status        string           `json:"status"`
	QueuePosition *int             `json:"queue_position,omitempty"`
	ImageDigest   *string          `json:"image_digest,omitempty"`
	ImageRef      *string          `json:"image_ref,omitempty"`
	Error         *string          `json:"error,omitempty"`
	Provenance    *BuildProvenance `json:"provenance,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	StartedAt     *time.Time       `json:"started_at,omitempty"`
	CompletedAt   *time.Time       `json:"completed_at,omitempty"`
	DurationMS    *int64           `json:"duration_ms,omitempty"`
}

// CreateBuildRequest represents a request to create a new build
type CreateBuildRequest struct {
	// Dockerfile content. Required if not included in the source tarball.
	// The Dockerfile specifies the runtime (e.g., FROM node:20-alpine).
	Dockerfile string `json:"dockerfile,omitempty"`

	// BaseImageDigest optionally pins the base image by digest for reproducibility
	BaseImageDigest string `json:"base_image_digest,omitempty"`

	// SourceHash is the SHA256 hash of the source tarball for verification
	SourceHash string `json:"source_hash,omitempty"`

	// BuildPolicy contains resource limits and network policy for the build
	BuildPolicy *BuildPolicy `json:"build_policy,omitempty"`

	// CacheScope is the tenant-specific cache key prefix for isolation
	CacheScope string `json:"cache_scope,omitempty"`

	// BuildArgs are ARG values to pass to the Dockerfile
	BuildArgs map[string]string `json:"build_args,omitempty"`

	// Secrets are secret references to inject during build
	Secrets []SecretRef `json:"secrets,omitempty"`
}

// BuildPolicy defines resource limits and network policy for a build
type BuildPolicy struct {
	// TimeoutSeconds is the maximum build duration (default: 600)
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// MemoryMB is the memory limit for the builder VM (default: 2048)
	MemoryMB int `json:"memory_mb,omitempty"`

	// CPUs is the number of vCPUs for the builder VM (default: 2)
	CPUs int `json:"cpus,omitempty"`

	// NetworkMode controls network access during build
	// "isolated" = no network, "egress" = outbound allowed
	NetworkMode string `json:"network_mode,omitempty"`

	// AllowedDomains restricts egress to specific domains (only when NetworkMode="egress")
	AllowedDomains []string `json:"allowed_domains,omitempty"`
}

// SecretRef references a secret to inject during build
type SecretRef struct {
	// ID is the secret identifier (used in --mount=type=secret,id=...)
	ID string `json:"id"`

	// EnvVar is the environment variable name to expose the secret as
	EnvVar string `json:"env_var,omitempty"`
}

// BuildProvenance records the inputs and toolchain used for a build
// This enables reproducibility verification and audit trails
type BuildProvenance struct {
	// BaseImageDigest is the pinned base image used
	BaseImageDigest string `json:"base_image_digest"`

	// SourceHash is the SHA256 of the source tarball
	SourceHash string `json:"source_hash"`

	// LockfileHashes maps lockfile names to their SHA256 hashes
	LockfileHashes map[string]string `json:"lockfile_hashes,omitempty"`

	// BuildkitVersion is the BuildKit version used
	BuildkitVersion string `json:"buildkit_version,omitempty"`

	// Timestamp is when the build completed
	Timestamp time.Time `json:"timestamp"`
}

// BuildConfig is the configuration passed to the builder VM via config disk
// This is read by the builder agent inside the guest
type BuildConfig struct {
	// JobID is the build job identifier
	JobID string `json:"job_id"`

	// Dockerfile content (if not provided in source tarball)
	Dockerfile string `json:"dockerfile,omitempty"`

	// BaseImageDigest optionally pins the base image
	BaseImageDigest string `json:"base_image_digest,omitempty"`

	// RegistryURL is where to push the built image
	RegistryURL string `json:"registry_url"`

	// RegistryToken is a short-lived JWT granting push access to specific repositories.
	// The builder agent uses this token to authenticate with the registry.
	RegistryToken string `json:"registry_token,omitempty"`

	// CacheScope is the tenant-specific cache key prefix
	CacheScope string `json:"cache_scope,omitempty"`

	// SourcePath is the path to source in the guest (typically /src)
	SourcePath string `json:"source_path"`

	// BuildArgs are ARG values for the Dockerfile
	BuildArgs map[string]string `json:"build_args,omitempty"`

	// Secrets are secret references to fetch from host
	Secrets []SecretRef `json:"secrets,omitempty"`

	// TimeoutSeconds is the build timeout
	TimeoutSeconds int `json:"timeout_seconds"`

	// NetworkMode is "isolated" or "egress"
	NetworkMode string `json:"network_mode"`
}

// BuildResult is returned by the builder agent after a build completes
type BuildResult struct {
	// Success indicates whether the build succeeded
	Success bool `json:"success"`

	// ImageDigest is the digest of the pushed image (only on success)
	ImageDigest string `json:"image_digest,omitempty"`

	// Error is the error message (only on failure)
	Error string `json:"error,omitempty"`

	// Logs is the full build log output
	Logs string `json:"logs,omitempty"`

	// Provenance records build inputs for reproducibility
	Provenance BuildProvenance `json:"provenance"`

	// DurationMS is the build duration in milliseconds
	DurationMS int64 `json:"duration_ms"`
}

// DefaultBuildPolicy returns the default build policy
func DefaultBuildPolicy() BuildPolicy {
	return BuildPolicy{
		TimeoutSeconds: 600,  // 10 minutes
		MemoryMB:       2048, // 2GB
		CPUs:           2,
		NetworkMode:    "egress", // Allow outbound for dependency downloads
	}
}

// ApplyDefaults fills in default values for a build policy
func (p *BuildPolicy) ApplyDefaults() {
	defaults := DefaultBuildPolicy()
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = defaults.TimeoutSeconds
	}
	if p.MemoryMB == 0 {
		p.MemoryMB = defaults.MemoryMB
	}
	if p.CPUs == 0 {
		p.CPUs = defaults.CPUs
	}
	if p.NetworkMode == "" {
		p.NetworkMode = defaults.NetworkMode
	}
}
