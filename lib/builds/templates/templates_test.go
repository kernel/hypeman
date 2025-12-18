package templates

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetGenerator(t *testing.T) {
	tests := []struct {
		runtime string
		wantErr bool
	}{
		{"nodejs20", false},
		{"python312", false},
		{"ruby", true},
		{"java", true},
		{"", true},
	}

	for _, tt := range tests {
		t.Run(tt.runtime, func(t *testing.T) {
			gen, err := GetGenerator(tt.runtime)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, gen)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, gen)
			}
		})
	}
}

func TestNodeJSGenerator_DetectLockfile(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()

	gen := &NodeJSGenerator{Version: "20"}

	// Default to npm when no lockfile
	manager, lockfile := gen.DetectLockfile(tmpDir)
	assert.Equal(t, "npm", manager)
	assert.Equal(t, "package-lock.json", lockfile)

	// Detect pnpm
	os.WriteFile(filepath.Join(tmpDir, "pnpm-lock.yaml"), []byte{}, 0644)
	manager, lockfile = gen.DetectLockfile(tmpDir)
	assert.Equal(t, "pnpm", manager)
	assert.Equal(t, "pnpm-lock.yaml", lockfile)

	// Remove pnpm, add yarn
	os.Remove(filepath.Join(tmpDir, "pnpm-lock.yaml"))
	os.WriteFile(filepath.Join(tmpDir, "yarn.lock"), []byte{}, 0644)
	manager, lockfile = gen.DetectLockfile(tmpDir)
	assert.Equal(t, "yarn", manager)
	assert.Equal(t, "yarn.lock", lockfile)
}

func TestNodeJSGenerator_Generate(t *testing.T) {
	tmpDir := t.TempDir()

	// Create package.json
	err := os.WriteFile(filepath.Join(tmpDir, "package.json"), []byte(`{"name": "test"}`), 0644)
	require.NoError(t, err)

	// Create package-lock.json
	err = os.WriteFile(filepath.Join(tmpDir, "package-lock.json"), []byte(`{}`), 0644)
	require.NoError(t, err)

	// Create index.js
	err = os.WriteFile(filepath.Join(tmpDir, "index.js"), []byte(`console.log("hello")`), 0644)
	require.NoError(t, err)

	gen := &NodeJSGenerator{Version: "20"}
	dockerfile, err := gen.Generate(tmpDir, "")
	require.NoError(t, err)

	// Check Dockerfile contents
	assert.Contains(t, dockerfile, "FROM node:20-alpine")
	assert.Contains(t, dockerfile, "npm ci")
	assert.Contains(t, dockerfile, "COPY package.json package-lock.json")
	assert.Contains(t, dockerfile, "CMD [\"node\", \"index.js\"]")
}

func TestNodeJSGenerator_GenerateWithCustomBase(t *testing.T) {
	tmpDir := t.TempDir()

	os.WriteFile(filepath.Join(tmpDir, "package.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(tmpDir, "package-lock.json"), []byte(`{}`), 0644)

	gen := &NodeJSGenerator{Version: "20"}
	dockerfile, err := gen.Generate(tmpDir, "node@sha256:abc123")
	require.NoError(t, err)

	assert.Contains(t, dockerfile, "FROM node@sha256:abc123")
}

func TestNodeJSGenerator_MissingPackageJson(t *testing.T) {
	tmpDir := t.TempDir()

	gen := &NodeJSGenerator{Version: "20"}
	_, err := gen.Generate(tmpDir, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "package.json not found")
}

func TestPythonGenerator_DetectLockfile(t *testing.T) {
	tmpDir := t.TempDir()

	gen := &PythonGenerator{Version: "3.12"}

	// Default to pip when no lockfile
	manager, lockfile := gen.DetectLockfile(tmpDir)
	assert.Equal(t, "pip", manager)
	assert.Equal(t, "requirements.txt", lockfile)

	// Detect poetry
	os.WriteFile(filepath.Join(tmpDir, "poetry.lock"), []byte{}, 0644)
	manager, lockfile = gen.DetectLockfile(tmpDir)
	assert.Equal(t, "poetry", manager)
	assert.Equal(t, "poetry.lock", lockfile)

	// Remove poetry, add pipenv
	os.Remove(filepath.Join(tmpDir, "poetry.lock"))
	os.WriteFile(filepath.Join(tmpDir, "Pipfile.lock"), []byte{}, 0644)
	manager, lockfile = gen.DetectLockfile(tmpDir)
	assert.Equal(t, "pipenv", manager)
	assert.Equal(t, "Pipfile.lock", lockfile)
}

func TestPythonGenerator_Generate(t *testing.T) {
	tmpDir := t.TempDir()

	// Create requirements.txt
	err := os.WriteFile(filepath.Join(tmpDir, "requirements.txt"), []byte("flask==2.0.0"), 0644)
	require.NoError(t, err)

	// Create main.py
	err = os.WriteFile(filepath.Join(tmpDir, "main.py"), []byte(`print("hello")`), 0644)
	require.NoError(t, err)

	gen := &PythonGenerator{Version: "3.12"}
	dockerfile, err := gen.Generate(tmpDir, "")
	require.NoError(t, err)

	assert.Contains(t, dockerfile, "FROM python:3.12-slim")
	assert.Contains(t, dockerfile, "pip install --no-cache-dir -r requirements.txt")
	assert.Contains(t, dockerfile, "COPY requirements.txt")
	assert.Contains(t, dockerfile, "CMD [\"python\", \"main.py\"]")
}

func TestPythonGenerator_GenerateWithHashes(t *testing.T) {
	tmpDir := t.TempDir()

	// Create requirements.txt with hashes
	err := os.WriteFile(filepath.Join(tmpDir, "requirements.txt"), []byte(`flask==2.0.0 --hash=sha256:abc123`), 0644)
	require.NoError(t, err)

	gen := &PythonGenerator{Version: "3.12"}
	dockerfile, err := gen.Generate(tmpDir, "")
	require.NoError(t, err)

	// Should use strict mode with hashes
	assert.Contains(t, dockerfile, "--require-hashes")
	assert.Contains(t, dockerfile, "--only-binary")
}

func TestPythonGenerator_MissingRequirements(t *testing.T) {
	tmpDir := t.TempDir()

	gen := &PythonGenerator{Version: "3.12"}
	_, err := gen.Generate(tmpDir, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "requirements.txt not found")
}

