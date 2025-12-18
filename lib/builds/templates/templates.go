// Package templates provides Dockerfile generation for different runtimes.
package templates

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Generator generates Dockerfiles for a specific runtime
type Generator interface {
	// Generate creates a Dockerfile for the given source directory
	Generate(sourceDir string, baseImageDigest string) (string, error)

	// DetectLockfile returns the detected lockfile type and path
	DetectLockfile(sourceDir string) (string, string)
}

// GetGenerator returns a Generator for the given runtime
func GetGenerator(runtime string) (Generator, error) {
	switch runtime {
	case "nodejs20":
		return &NodeJSGenerator{Version: "20"}, nil
	case "python312":
		return &PythonGenerator{Version: "3.12"}, nil
	default:
		return nil, fmt.Errorf("unsupported runtime: %s", runtime)
	}
}

// NodeJSGenerator generates Dockerfiles for Node.js applications
type NodeJSGenerator struct {
	Version string
}

// DetectLockfile detects which package manager lockfile is present
func (g *NodeJSGenerator) DetectLockfile(sourceDir string) (string, string) {
	lockfiles := []struct {
		name    string
		manager string
	}{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"package-lock.json", "npm"},
	}

	for _, lf := range lockfiles {
		path := filepath.Join(sourceDir, lf.name)
		if _, err := os.Stat(path); err == nil {
			return lf.manager, lf.name
		}
	}

	return "npm", "package-lock.json"
}

// Generate creates a Dockerfile for a Node.js application
func (g *NodeJSGenerator) Generate(sourceDir string, baseImageDigest string) (string, error) {
	manager, lockfile := g.DetectLockfile(sourceDir)

	// Determine base image
	baseImage := baseImageDigest
	if baseImage == "" {
		baseImage = fmt.Sprintf("node:%s-alpine", g.Version)
	}

	// Determine install command based on package manager
	var installCmd string
	switch manager {
	case "pnpm":
		installCmd = "corepack enable && pnpm install --frozen-lockfile"
	case "yarn":
		installCmd = "yarn install --frozen-lockfile"
	default:
		installCmd = "npm ci"
	}

	// Check if package.json exists
	if _, err := os.Stat(filepath.Join(sourceDir, "package.json")); err != nil {
		return "", fmt.Errorf("package.json not found in source directory")
	}

	// Detect entry point
	entryPoint := detectNodeEntryPoint(sourceDir)

	dockerfile := fmt.Sprintf(`FROM %s

WORKDIR /app

# Copy dependency files first (cache layer)
COPY package.json %s ./

# Install dependencies (strict mode from lockfile)
RUN %s

# Copy application source
COPY . .

# Default command
CMD ["node", "%s"]
`, baseImage, lockfile, installCmd, entryPoint)

	return dockerfile, nil
}

// detectNodeEntryPoint tries to detect the entry point for a Node.js app
func detectNodeEntryPoint(sourceDir string) string {
	// Check common entry points
	candidates := []string{"index.js", "src/index.js", "main.js", "app.js", "server.js"}
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(sourceDir, candidate)); err == nil {
			return candidate
		}
	}
	return "index.js"
}

// PythonGenerator generates Dockerfiles for Python applications
type PythonGenerator struct {
	Version string
}

// DetectLockfile detects which Python dependency file is present
func (g *PythonGenerator) DetectLockfile(sourceDir string) (string, string) {
	lockfiles := []struct {
		name    string
		manager string
	}{
		{"poetry.lock", "poetry"},
		{"Pipfile.lock", "pipenv"},
		{"requirements.txt", "pip"},
	}

	for _, lf := range lockfiles {
		path := filepath.Join(sourceDir, lf.name)
		if _, err := os.Stat(path); err == nil {
			return lf.manager, lf.name
		}
	}

	return "pip", "requirements.txt"
}

// Generate creates a Dockerfile for a Python application
func (g *PythonGenerator) Generate(sourceDir string, baseImageDigest string) (string, error) {
	manager, lockfile := g.DetectLockfile(sourceDir)

	// Determine base image
	baseImage := baseImageDigest
	if baseImage == "" {
		baseImage = fmt.Sprintf("python:%s-slim", g.Version)
	}

	var installCmd string
	var copyFiles string

	switch manager {
	case "poetry":
		// Poetry requires pyproject.toml and poetry.lock
		copyFiles = "pyproject.toml poetry.lock"
		installCmd = `pip install poetry && \
    poetry config virtualenvs.create false && \
    poetry install --no-dev --no-interaction --no-ansi`
	case "pipenv":
		copyFiles = "Pipfile Pipfile.lock"
		installCmd = `pip install pipenv && \
    pipenv install --system --deploy --ignore-pipfile`
	default:
		// Check if requirements.txt has hashes for strict mode
		hasHashes := checkRequirementsHasHashes(sourceDir)
		copyFiles = "requirements.txt"
		if hasHashes {
			// Strict mode: require hashes, prefer binary packages
			installCmd = "pip install --require-hashes --only-binary :all: -r requirements.txt"
		} else {
			installCmd = "pip install --no-cache-dir -r requirements.txt"
		}
	}

	// Check if lockfile exists
	if _, err := os.Stat(filepath.Join(sourceDir, lockfile)); err != nil {
		return "", fmt.Errorf("%s not found in source directory", lockfile)
	}

	// Detect entry point
	entryPoint := detectPythonEntryPoint(sourceDir)

	dockerfile := fmt.Sprintf(`FROM %s

WORKDIR /app

# Copy dependency files first (cache layer)
COPY %s ./

# Install dependencies
RUN %s

# Copy application source
COPY . .

# Default command
CMD ["python", "%s"]
`, baseImage, copyFiles, installCmd, entryPoint)

	return dockerfile, nil
}

// checkRequirementsHasHashes checks if requirements.txt contains hash pins
func checkRequirementsHasHashes(sourceDir string) bool {
	reqPath := filepath.Join(sourceDir, "requirements.txt")
	data, err := os.ReadFile(reqPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "--hash=")
}

// detectPythonEntryPoint tries to detect the entry point for a Python app
func detectPythonEntryPoint(sourceDir string) string {
	// Check common entry points
	candidates := []string{"main.py", "app.py", "run.py", "server.py", "src/main.py"}
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(sourceDir, candidate)); err == nil {
			return candidate
		}
	}
	return "main.py"
}

