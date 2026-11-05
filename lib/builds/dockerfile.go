package builds

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

var fromImagePattern = regexp.MustCompile(`(?i)^(\s*FROM\s+(?:--\S+\s+)*)(\S+)`)

// rewriteMirroredFROMs routes cached non-Docker-Hub bases through the host's
// registry so the guest only needs its scoped local-registry token.
func rewriteMirroredFROMs(content string, replacements map[string]string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		match := fromImagePattern.FindStringSubmatchIndex(line)
		if match == nil {
			continue
		}
		start, end := match[4], match[5]
		if replacement, ok := replacements[normalizeImageRef(line[start:end])]; ok {
			lines[i] = line[:start] + replacement + line[end:]
		}
	}
	return strings.Join(lines, "\n")
}

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

// normalizeImageRef normalizes a Docker image reference to match the local
// registry path that BuildKit mirror requests will use. Official Docker Hub
// images keep the library/ prefix (e.g. "node:20-alpine" → "library/node:20-alpine")
// because BuildKit requests them as /v2/library/node/manifests/....
// Non-Docker Hub images keep the full registry path.
//
// This is consistent with normalizeToLocalRef in lib/images/mirror.go, which
// controls where mirrored images are pushed.
func normalizeImageRef(ref string) string {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		// Fall back to basic normalization if parsing fails
		return strings.TrimPrefix(ref, "docker.io/")
	}

	// Get canonicalized repository (e.g. "index.docker.io/library/node")
	repo := parsed.Context().String()

	// Strip index.docker.io/ prefix (canonical form of docker.io)
	repo = strings.TrimPrefix(repo, "index.docker.io/")
	repo = strings.TrimPrefix(repo, "docker.io/")

	// Keep library/ prefix — BuildKit mirror requests use it for official images

	// Build the tag or digest suffix
	var suffix string
	if tag, ok := parsed.(name.Tag); ok {
		suffix = ":" + tag.TagStr()
	} else if dig, ok := parsed.(name.Digest); ok {
		suffix = "@" + dig.DigestStr()
	}

	return repo + suffix
}

// writeSourceWithDockerfile prepares a build input without modifying the stored
// source archive. Older builder images prefer the archive's Dockerfile to config.
func writeSourceWithDockerfile(source io.Reader, destination io.Writer, dockerfile string) error {
	reader, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer reader.Close()
	compressed := gzip.NewWriter(destination)
	defer compressed.Close()
	writer := tar.NewWriter(compressed)
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Name == "Dockerfile" || header.Name == "./Dockerfile" {
			continue
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if _, err := io.Copy(writer, archive); err != nil {
			return err
		}
	}
	if err := writer.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0644, Size: int64(len(dockerfile))}); err != nil {
		return err
	}
	if _, err := io.WriteString(writer, dockerfile); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return compressed.Close()
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
