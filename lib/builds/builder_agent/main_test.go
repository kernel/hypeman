package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBaseBuildctlArgsSeparatesDockerfileFromContext(t *testing.T) {
	config := &BuildConfig{SourcePath: "/tmp/source"}

	got := baseBuildctlArgs(config, "/tmp/dockerfile", "type=image,name=example")
	want := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/tmp/source",
		"--local", "dockerfile=/tmp/dockerfile",
		"--output", "type=image,name=example",
		"--metadata-file", "/tmp/build-metadata.json",
	}

	if !slices.Equal(got, want) {
		t.Fatalf("unexpected buildctl args:\n got: %q\nwant: %q", got, want)
	}
}

func TestPrepareDockerfileKeepsConfiguredDockerfileOutsideSource(t *testing.T) {
	sourceDir := t.TempDir()
	dockerfile := "FROM alpine:3.21\nCOPY . /app\n"

	dockerfileDir, cleanup, err := prepareDockerfile(&BuildConfig{
		SourcePath: sourceDir,
		Dockerfile: dockerfile,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	if dockerfileDir == sourceDir {
		t.Fatal("configured Dockerfile was written into the source directory")
	}
	if _, err := os.Stat(filepath.Join(sourceDir, "Dockerfile")); !os.IsNotExist(err) {
		t.Fatalf("expected source directory to remain unchanged, got %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dockerfileDir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != dockerfile {
		t.Fatalf("unexpected Dockerfile content: %q", content)
	}
}

func TestPrepareDockerfileUsesSourceDockerfile(t *testing.T) {
	sourceDir := t.TempDir()
	dockerfilePath := filepath.Join(sourceDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte("FROM source\n"), 0644); err != nil {
		t.Fatal(err)
	}

	dockerfileDir, cleanup, err := prepareDockerfile(&BuildConfig{
		SourcePath: sourceDir,
		Dockerfile: "FROM config\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	if dockerfileDir != sourceDir {
		t.Fatalf("expected source Dockerfile directory %q, got %q", sourceDir, dockerfileDir)
	}
	content, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "FROM source\n" {
		t.Fatalf("source Dockerfile was overwritten: %q", content)
	}
}

func TestPrepareDockerfileRequiresDockerfile(t *testing.T) {
	_, _, err := prepareDockerfile(&BuildConfig{SourcePath: t.TempDir()})
	if err == nil {
		t.Fatal("expected missing Dockerfile to fail")
	}
}
