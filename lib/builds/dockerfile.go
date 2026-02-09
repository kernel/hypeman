package builds

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ParseDockerfileFROMs extracts and deduplicates base image references from
// Dockerfile content. It reuses the same parsing logic as the builder agent's
// rewriteDockerfileFROMs: split lines, find FROM, skip flags/comments/scratch,
// normalize refs. Inter-stage references (FROM builder) and variable references
// (${VAR}) are skipped since they can't be resolved at parse time.
func ParseDockerfileFROMs(content string) []string {
	lines := strings.Split(content, "\n")

	// Track stage names so we can skip inter-stage FROM references
	stageNames := make(map[string]bool)
	seen := make(map[string]bool)
	var refs []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip empty lines and comments
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Check for FROM instruction (case insensitive)
		upper := strings.ToUpper(trimmed)
		if !strings.HasPrefix(upper, "FROM ") {
			continue
		}

		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			continue
		}

		// Find the image reference (skip FROM and any flags like --platform)
		imageIdx := 1
		for imageIdx < len(parts) && strings.HasPrefix(parts[imageIdx], "--") {
			imageIdx++
		}
		if imageIdx >= len(parts) {
			continue
		}

		imageRef := parts[imageIdx]

		// Record AS alias if present
		for j := imageIdx + 1; j < len(parts)-1; j++ {
			if strings.EqualFold(parts[j], "AS") {
				stageNames[strings.ToLower(parts[j+1])] = true
				break
			}
		}

		// Skip scratch
		if imageRef == "scratch" {
			continue
		}

		// Skip inter-stage references (e.g. FROM builder)
		if stageNames[strings.ToLower(imageRef)] {
			continue
		}

		// Skip variable references that can't be resolved
		if strings.Contains(imageRef, "${") {
			continue
		}

		// Normalize the image reference (same logic as builder agent)
		normalized := normalizeImageRef(imageRef)

		if !seen[normalized] {
			seen[normalized] = true
			refs = append(refs, normalized)
		}
	}

	return refs
}

// normalizeImageRef normalizes a Docker image reference by stripping
// docker.io/ and library/ prefixes. This matches the builder agent's
// normalizeImageRef function.
func normalizeImageRef(ref string) string {
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "library/")
	return ref
}

// ExtractDockerfileFromTarball reads just the Dockerfile entry from a .tar.gz
// archive and returns its content as a string. It looks for entries named
// "Dockerfile" or "./Dockerfile" at the root of the archive.
func ExtractDockerfileFromTarball(tarballPath string) (string, error) {
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("create gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar entry: %w", err)
		}

		// Match Dockerfile at root (with or without ./ prefix)
		name := filepath.Clean(hdr.Name)
		if name == "Dockerfile" {
			data, err := io.ReadAll(tr)
			if err != nil {
				return "", fmt.Errorf("read Dockerfile from tarball: %w", err)
			}
			return string(data), nil
		}
	}

	return "", fmt.Errorf("Dockerfile not found in tarball")
}
