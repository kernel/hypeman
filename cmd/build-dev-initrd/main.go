package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci/oci/cas/dir"
	"github.com/opencontainers/umoci/oci/casext"
)

func main() {
	ctx := context.Background()

	// Get project root
	projectRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting working directory: %v\n", err)
		os.Exit(1)
	}

	initrdDir := filepath.Join(projectRoot, "lib/system/initrd")
	
	// Step 1: Build Docker image with OCI output (like GitHub workflow)
	fmt.Println("Building initrd Docker image with OCI format...")
	ociDir := filepath.Join(os.TempDir(), "hypeman-initrd-oci-dev")
	os.RemoveAll(ociDir)
	os.MkdirAll(ociDir, 0755)
	
	// Use docker buildx to build directly to OCI format
	// This matches the GitHub workflow approach
	cmd := exec.Command("docker", "buildx", "build",
		"--output", fmt.Sprintf("type=oci,dest=%s/image.tar,oci-mediatypes=true", ociDir),
		"--platform", "linux/amd64",
		".")
	cmd.Dir = initrdDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error building Docker image: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓ Docker image built in OCI format")

	// Step 2: Extract OCI tar to directory (umoci expects a directory layout)
	fmt.Println("\nExtracting OCI layout...")
	ociLayoutDir := filepath.Join(ociDir, "layout")
	os.MkdirAll(ociLayoutDir, 0755)
	
	cmd = exec.Command("tar", "-xf", filepath.Join(ociDir, "image.tar"), "-C", ociLayoutDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error extracting OCI tar: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓ Extracted OCI layout")

	// Step 3: Use existing system manager to build initrd from OCI directory
	fmt.Println("\nBuilding initrd using existing pipeline...")
	pathsConfig := paths.New("/var/lib/hypeman")
	version := system.InitrdVersion("v2.0.2-dev")
	arch := system.GetArch()

	// Create temp directory for building
	tempDir, err := os.MkdirTemp("", "hypeman-initrd-build-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tempDir)

	rootfsDir := filepath.Join(tempDir, "rootfs")

	// Create OCI client using our locally built OCI layout as the cache
	// This way the image is already "cached" and won't try to pull from remote
	ociClient, err := images.NewOCIClient(ociLayoutDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating OCI client: %v\n", err)
		os.Exit(1)
	}

	// Read the index.json to get the manifest digest
	indexData, err := os.ReadFile(filepath.Join(ociLayoutDir, "index.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading index.json: %v\n", err)
		os.Exit(1)
	}

	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexData, &index); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing index.json: %v\n", err)
		os.Exit(1)
	}

	if len(index.Manifests) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found in index.json\n")
		os.Exit(1)
	}

	digest := index.Manifests[0].Digest
	fmt.Printf("  Using manifest: %s\n", digest)

	// Tag the manifest in the OCI layout so the OCI client can find it
	// The OCI client expects tags in the format that digestToLayoutTag produces (just the hex part)
	layoutTag := strings.TrimPrefix(digest, "sha256:")
	
	// Use umoci library to create the tag
	if err := tagManifestInOCI(ociLayoutDir, digest, layoutTag); err != nil {
		fmt.Fprintf(os.Stderr, "Error tagging manifest in OCI layout: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Tagged as: %s\n", layoutTag)

	// Now the OCI client will find it in the cache and won't try to pull
	// We pass a dummy imageRef since it won't be used (image is already cached)
	if err := ociClient.PullAndUnpack(ctx, "local/dev", digest, rootfsDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error unpacking OCI image: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓ Unpacked OCI image")

	// Inject init script
	fmt.Println("\nInjecting init script...")
	initScript := system.GenerateInitScript(version)
	initPath := filepath.Join(rootfsDir, "init")
	if err := os.WriteFile(initPath, []byte(initScript), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing init script: %v\n", err)
		os.Exit(1)
	}

	// Package as cpio.gz (initramfs format)
	fmt.Println("Packaging as initrd...")
	outputPath := pathsConfig.SystemInitrd(string(version), arch)
	os.MkdirAll(filepath.Dir(outputPath), 0755)
	
	if _, err := images.ExportRootfs(rootfsDir, outputPath, images.FormatCpio); err != nil {
		fmt.Fprintf(os.Stderr, "Error exporting initrd: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n✓ Dev initrd built successfully!")
	fmt.Printf("  Location: %s\n", outputPath)
	fmt.Printf("  OCI cache: %s (can be deleted)\n", ociDir)
	fmt.Println("\nTo use it, update lib/system/versions.go:")
	fmt.Println("  DefaultInitrdVersion = InitrdVersion(\"v2.0.2-dev\")")
}

// tagManifestInOCI tags a manifest digest with a tag name in an OCI layout
func tagManifestInOCI(ociLayoutDir, digestStr, tag string) error {
	casEngine, err := dir.Open(ociLayoutDir)
	if err != nil {
		return fmt.Errorf("open OCI layout: %w", err)
	}
	defer casEngine.Close()

	engine := casext.NewEngine(casEngine)

	// Read the index to find the manifest descriptor
	indexData, err := os.ReadFile(filepath.Join(ociLayoutDir, "index.json"))
	if err != nil {
		return fmt.Errorf("read index.json: %w", err)
	}

	var index struct {
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexData, &index); err != nil {
		return fmt.Errorf("parse index.json: %w", err)
	}

	// Find the manifest descriptor matching our digest
	var manifestDesc *v1.Descriptor
	for _, m := range index.Manifests {
		if m.Digest == digestStr {
			manifestDesc = &v1.Descriptor{
				MediaType: m.MediaType,
				Digest:    digest.Digest(digestStr),
				Size:      m.Size,
			}
			break
		}
	}

	if manifestDesc == nil {
		return fmt.Errorf("manifest %s not found in index", digestStr)
	}

	// Update the reference to create the tag
	if err := engine.UpdateReference(context.Background(), tag, *manifestDesc); err != nil {
		return fmt.Errorf("update reference: %w", err)
	}

	return nil
}

