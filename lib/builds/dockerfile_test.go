package builds

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestParseDockerfileFROMs_SingleFROM(t *testing.T) {
	content := `FROM onkernel/nodejs22-base:0.1.1
RUN echo hello
`
	refs := ParseDockerfileFROMs(content)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d: %v", len(refs), refs)
	}
	if refs[0] != "onkernel/nodejs22-base:0.1.1" {
		t.Errorf("expected onkernel/nodejs22-base:0.1.1, got %s", refs[0])
	}
}

func TestParseDockerfileFROMs_MultiStage(t *testing.T) {
	content := `FROM golang:1.21 AS builder
RUN go build -o /app .

FROM alpine:3.21
COPY --from=builder /app /app
`
	refs := ParseDockerfileFROMs(content)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d: %v", len(refs), refs)
	}
	if refs[0] != "library/golang:1.21" {
		t.Errorf("expected library/golang:1.21, got %s", refs[0])
	}
	if refs[1] != "library/alpine:3.21" {
		t.Errorf("expected library/alpine:3.21, got %s", refs[1])
	}
}

func TestParseDockerfileFROMs_DockerIONormalization(t *testing.T) {
	content := `FROM docker.io/library/alpine:3.21
`
	refs := ParseDockerfileFROMs(content)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d: %v", len(refs), refs)
	}
	if refs[0] != "library/alpine:3.21" {
		t.Errorf("expected library/alpine:3.21, got %s", refs[0])
	}
}

func TestParseDockerfileFROMs_PlatformFlag(t *testing.T) {
	content := `FROM --platform=linux/amd64 node:20-alpine
RUN npm install
`
	refs := ParseDockerfileFROMs(content)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d: %v", len(refs), refs)
	}
	if refs[0] != "library/node:20-alpine" {
		t.Errorf("expected library/node:20-alpine, got %s", refs[0])
	}
}

func TestParseDockerfileFROMs_SkipScratch(t *testing.T) {
	content := `FROM golang:1.21 AS builder
RUN go build -o /app .

FROM scratch
COPY --from=builder /app /app
`
	refs := ParseDockerfileFROMs(content)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d: %v", len(refs), refs)
	}
	if refs[0] != "library/golang:1.21" {
		t.Errorf("expected library/golang:1.21, got %s", refs[0])
	}
}

func TestParseDockerfileFROMs_SkipStageReferences(t *testing.T) {
	content := `FROM node:20 AS deps
RUN npm ci

FROM node:20 AS builder
COPY --from=deps /app/node_modules ./node_modules
RUN npm run build

FROM builder
CMD ["node", "dist/index.js"]
`
	refs := ParseDockerfileFROMs(content)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref (deduplicated), got %d: %v", len(refs), refs)
	}
	if refs[0] != "library/node:20" {
		t.Errorf("expected library/node:20, got %s", refs[0])
	}
}

func TestParseDockerfileFROMs_SkipVariableReferences(t *testing.T) {
	content := `ARG BASE_IMAGE=node:20
FROM ${BASE_IMAGE}
RUN echo hello
`
	refs := ParseDockerfileFROMs(content)
	if len(refs) != 0 {
		t.Fatalf("expected 0 refs (variable), got %d: %v", len(refs), refs)
	}
}

func TestParseDockerfileFROMs_Deduplication(t *testing.T) {
	content := `FROM alpine:3.21 AS stage1
RUN echo one

FROM alpine:3.21 AS stage2
RUN echo two
`
	refs := ParseDockerfileFROMs(content)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref (deduplicated), got %d: %v", len(refs), refs)
	}
	if refs[0] != "library/alpine:3.21" {
		t.Errorf("expected library/alpine:3.21, got %s", refs[0])
	}
}

func TestParseDockerfileFROMs_CommentsAndEmptyLines(t *testing.T) {
	content := `# Build stage
FROM golang:1.21

# This is a comment
# FROM fake:image

RUN echo hello
`
	refs := ParseDockerfileFROMs(content)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d: %v", len(refs), refs)
	}
	if refs[0] != "library/golang:1.21" {
		t.Errorf("expected library/golang:1.21, got %s", refs[0])
	}
}

func TestExtractDockerfileFromTarball(t *testing.T) {
	// Create a temp tarball with a Dockerfile
	dir := t.TempDir()
	tarballPath := filepath.Join(dir, "source.tar.gz")

	dockerfileContent := "FROM alpine:3.21\nRUN echo hello\n"
	createTarball(t, tarballPath, map[string]string{
		"Dockerfile": dockerfileContent,
		"main.go":    "package main\n",
	})

	content, err := ExtractDockerfileFromTarball(tarballPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != dockerfileContent {
		t.Errorf("expected %q, got %q", dockerfileContent, content)
	}
}

func TestExtractDockerfileFromTarball_NotFound(t *testing.T) {
	dir := t.TempDir()
	tarballPath := filepath.Join(dir, "source.tar.gz")

	createTarball(t, tarballPath, map[string]string{
		"main.go": "package main\n",
	})

	_, err := ExtractDockerfileFromTarball(tarballPath)
	if err == nil {
		t.Fatal("expected error for missing Dockerfile")
	}
}

func TestExtractDockerfileFromTarball_DotSlashPrefix(t *testing.T) {
	dir := t.TempDir()
	tarballPath := filepath.Join(dir, "source.tar.gz")

	dockerfileContent := "FROM node:20\nRUN npm install\n"
	createTarball(t, tarballPath, map[string]string{
		"./Dockerfile": dockerfileContent,
	})

	content, err := ExtractDockerfileFromTarball(tarballPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != dockerfileContent {
		t.Errorf("expected %q, got %q", dockerfileContent, content)
	}
}

// createTarball creates a .tar.gz file with the given files (name -> content).
func createTarball(t *testing.T, path string, files map[string]string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tarball file: %v", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header for %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar content for %s: %v", name, err)
		}
	}
}
