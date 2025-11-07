package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/oci/layout"
	"github.com/containers/image/v5/signature"
	"github.com/opencontainers/image-spec/specs-go/v1"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/umoci/oci/cas/dir"
	"github.com/opencontainers/umoci/oci/casext"
	"github.com/opencontainers/umoci/oci/layer"
)

// OCIClient handles OCI image operations without requiring Docker daemon
type OCIClient struct {
	cacheDir string
}

// NewOCIClient creates a new OCI client
func NewOCIClient(cacheDir string) (*OCIClient, error) {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &OCIClient{cacheDir: cacheDir}, nil
}

// Close closes the OCI client (no-op for now)
func (c *OCIClient) Close() error {
	return nil
}

// pullAndExport pulls an OCI image and exports its rootfs to a directory
func (c *OCIClient) pullAndExport(ctx context.Context, imageRef, exportDir string) (*containerMetadata, error) {
	// Create temporary OCI layout directory
	ociLayoutDir := filepath.Join(c.cacheDir, fmt.Sprintf("oci-layout-%d", os.Getpid()))
	if err := os.MkdirAll(ociLayoutDir, 0755); err != nil {
		return nil, fmt.Errorf("create oci layout dir: %w", err)
	}
	defer os.RemoveAll(ociLayoutDir)

	// Pull image to OCI layout
	if err := c.pullToOCILayout(ctx, imageRef, ociLayoutDir); err != nil {
		return nil, fmt.Errorf("pull to oci layout: %w", err)
	}

	// Extract metadata from OCI config
	meta, err := c.extractOCIMetadata(ociLayoutDir)
	if err != nil {
		return nil, fmt.Errorf("extract metadata: %w", err)
	}

	// Unpack layers to export directory
	if err := c.unpackLayers(ctx, ociLayoutDir, exportDir); err != nil {
		return nil, fmt.Errorf("unpack layers: %w", err)
	}

	return meta, nil
}

// pullToOCILayout pulls an image from a registry to an OCI layout directory
func (c *OCIClient) pullToOCILayout(ctx context.Context, imageRef, ociLayoutDir string) error {
	// Parse source reference (docker://...)
	srcRef, err := docker.ParseReference("//" + imageRef)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}

	// Create destination reference (OCI layout)
	destRef, err := layout.ParseReference(ociLayoutDir + ":latest")
	if err != nil {
		return fmt.Errorf("parse oci layout reference: %w", err)
	}

	// Create policy context (allow all)
	policyContext, err := signature.NewPolicyContext(&signature.Policy{
		Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()},
	})
	if err != nil {
		return fmt.Errorf("create policy context: %w", err)
	}
	defer policyContext.Destroy()

	// Pull image
	_, err = copy.Image(ctx, policyContext, destRef, srcRef, &copy.Options{
		ReportWriter: os.Stdout,
	})
	if err != nil {
		return fmt.Errorf("copy image: %w", err)
	}

	return nil
}

// extractOCIMetadata reads metadata from OCI layout config.json
func (c *OCIClient) extractOCIMetadata(ociLayoutDir string) (*containerMetadata, error) {
	// Open the OCI layout
	casEngine, err := dir.Open(ociLayoutDir)
	if err != nil {
		return nil, fmt.Errorf("open oci layout: %w", err)
	}
	defer casEngine.Close()

	engine := casext.NewEngine(casEngine)

	// Get image reference (we tagged it as "latest")
	descriptorPaths, err := engine.ResolveReference(context.Background(), "latest")
	if err != nil {
		return nil, fmt.Errorf("resolve reference: %w", err)
	}

	if len(descriptorPaths) == 0 {
		return nil, fmt.Errorf("no image found in oci layout")
	}

	// Get the manifest
	manifestBlob, err := engine.FromDescriptor(context.Background(), descriptorPaths[0].Descriptor())
	if err != nil {
		return nil, fmt.Errorf("get manifest: %w", err)
	}

	// casext automatically parses manifests, so Data is already a v1.Manifest
	manifest, ok := manifestBlob.Data.(v1.Manifest)
	if !ok {
		return nil, fmt.Errorf("manifest data is not v1.Manifest (got %T)", manifestBlob.Data)
	}

	// Get the config blob
	configBlob, err := engine.FromDescriptor(context.Background(), manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("get config: %w", err)
	}

	// casext automatically parses config, so Data is already a v1.Image
	config, ok := configBlob.Data.(v1.Image)
	if !ok {
		return nil, fmt.Errorf("config data is not v1.Image (got %T)", configBlob.Data)
	}

	// Extract metadata
	meta := &containerMetadata{
		Entrypoint: config.Config.Entrypoint,
		Cmd:        config.Config.Cmd,
		Env:        make(map[string]string),
		WorkingDir: config.Config.WorkingDir,
	}

	// Parse environment variables
	for _, env := range config.Config.Env {
		for i := 0; i < len(env); i++ {
			if env[i] == '=' {
				key := env[:i]
				val := env[i+1:]
				meta.Env[key] = val
				break
			}
		}
	}

	return meta, nil
}

// unpackLayers unpacks all OCI layers to a target directory using umoci
func (c *OCIClient) unpackLayers(ctx context.Context, ociLayoutDir, targetDir string) error {
	// Open OCI layout
	casEngine, err := dir.Open(ociLayoutDir)
	if err != nil {
		return fmt.Errorf("open oci layout: %w", err)
	}
	defer casEngine.Close()

	engine := casext.NewEngine(casEngine)

	// Get the manifest descriptor for "latest" tag
	descriptorPaths, err := engine.ResolveReference(context.Background(), "latest")
	if err != nil {
		return fmt.Errorf("resolve reference: %w", err)
	}

	if len(descriptorPaths) == 0 {
		return fmt.Errorf("no image found")
	}

	// Get the manifest blob
	manifestBlob, err := engine.FromDescriptor(context.Background(), descriptorPaths[0].Descriptor())
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}

	// casext automatically parses manifests
	manifest, ok := manifestBlob.Data.(v1.Manifest)
	if !ok {
		return fmt.Errorf("manifest data is not v1.Manifest (got %T)", manifestBlob.Data)
	}

	// Pre-create target directory (umoci needs it to exist)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	// Unpack layers using umoci's layer package with rootless mode
	// Map container UIDs to current user's UID (identity mapping)
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	
	unpackOpts := &layer.UnpackOptions{
		OnDiskFormat: layer.DirRootfs{
			MapOptions: layer.MapOptions{
				Rootless: true, // Don't fail on chown errors
				UIDMappings: []rspec.LinuxIDMapping{
					{HostID: uid, ContainerID: 0, Size: 1}, // Map container root to current user
				},
				GIDMappings: []rspec.LinuxIDMapping{
					{HostID: gid, ContainerID: 0, Size: 1}, // Map container root group to current user group
				},
			},
		},
	}
	
	err = layer.UnpackRootfs(context.Background(), casEngine, targetDir, manifest, unpackOpts)
	if err != nil {
		return fmt.Errorf("unpack rootfs: %w", err)
	}

	return nil
}

// containerMetadata holds extracted container metadata
type containerMetadata struct {
	Entrypoint []string
	Cmd        []string
	Env        map[string]string
	WorkingDir string
}

