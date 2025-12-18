// Package main implements the builder agent that runs inside builder microVMs.
// It reads build configuration from the config disk, runs BuildKit to build
// the image, and reports results back to the host via vsock.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mdlayher/vsock"
)

const (
	configPath     = "/config/build.json"
	vsockPort      = 5001 // Build agent port (different from exec agent)
	hostCID        = 2    // VMADDR_CID_HOST
)

// BuildConfig matches the BuildConfig type from lib/builds/types.go
type BuildConfig struct {
	JobID           string            `json:"job_id"`
	Runtime         string            `json:"runtime"`
	BaseImageDigest string            `json:"base_image_digest,omitempty"`
	RegistryURL     string            `json:"registry_url"`
	CacheScope      string            `json:"cache_scope,omitempty"`
	SourcePath      string            `json:"source_path"`
	Dockerfile      string            `json:"dockerfile,omitempty"`
	BuildArgs       map[string]string `json:"build_args,omitempty"`
	Secrets         []SecretRef       `json:"secrets,omitempty"`
	TimeoutSeconds  int               `json:"timeout_seconds"`
	NetworkMode     string            `json:"network_mode"`
}

// SecretRef references a secret to inject during build
type SecretRef struct {
	ID     string `json:"id"`
	EnvVar string `json:"env_var,omitempty"`
}

// BuildResult is sent back to the host
type BuildResult struct {
	Success     bool            `json:"success"`
	ImageDigest string          `json:"image_digest,omitempty"`
	Error       string          `json:"error,omitempty"`
	Logs        string          `json:"logs,omitempty"`
	Provenance  BuildProvenance `json:"provenance"`
	DurationMS  int64           `json:"duration_ms"`
}

// BuildProvenance records build inputs
type BuildProvenance struct {
	BaseImageDigest  string            `json:"base_image_digest"`
	SourceHash       string            `json:"source_hash"`
	LockfileHashes   map[string]string `json:"lockfile_hashes,omitempty"`
	ToolchainVersion string            `json:"toolchain_version,omitempty"`
	BuildkitVersion  string            `json:"buildkit_version,omitempty"`
	Timestamp        time.Time         `json:"timestamp"`
}

// VsockMessage is the envelope for vsock communication
type VsockMessage struct {
	Type   string          `json:"type"`
	Result *BuildResult    `json:"result,omitempty"`
	Log    string          `json:"log,omitempty"`
}

func main() {
	start := time.Now()
	var logs bytes.Buffer
	logWriter := io.MultiWriter(os.Stdout, &logs)

	log.SetOutput(logWriter)
	log.Println("=== Builder Agent Starting ===")

	// Load build config
	config, err := loadConfig()
	if err != nil {
		sendResult(BuildResult{
			Success:    false,
			Error:      fmt.Sprintf("load config: %v", err),
			Logs:       logs.String(),
			DurationMS: time.Since(start).Milliseconds(),
		})
		os.Exit(1)
	}
	log.Printf("Job: %s, Runtime: %s", config.JobID, config.Runtime)

	// Setup timeout context
	ctx := context.Background()
	if config.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(config.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	// Fetch secrets from host if needed
	if err := fetchSecrets(ctx, config.Secrets); err != nil {
		sendResult(BuildResult{
			Success:    false,
			Error:      fmt.Sprintf("fetch secrets: %v", err),
			Logs:       logs.String(),
			DurationMS: time.Since(start).Milliseconds(),
		})
		os.Exit(1)
	}

	// Generate Dockerfile if not provided
	dockerfile := config.Dockerfile
	if dockerfile == "" {
		dockerfile, err = generateDockerfile(config)
		if err != nil {
			sendResult(BuildResult{
				Success:    false,
				Error:      fmt.Sprintf("generate dockerfile: %v", err),
				Logs:       logs.String(),
				DurationMS: time.Since(start).Milliseconds(),
			})
			os.Exit(1)
		}
		// Write generated Dockerfile
		dockerfilePath := filepath.Join(config.SourcePath, "Dockerfile")
		if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0644); err != nil {
			sendResult(BuildResult{
				Success:    false,
				Error:      fmt.Sprintf("write dockerfile: %v", err),
				Logs:       logs.String(),
				DurationMS: time.Since(start).Milliseconds(),
			})
			os.Exit(1)
		}
		log.Println("Generated Dockerfile for runtime:", config.Runtime)
	}

	// Compute provenance
	provenance := computeProvenance(config)

	// Run the build
	log.Println("=== Starting Build ===")
	digest, buildLogs, err := runBuild(ctx, config, logWriter)
	logs.WriteString(buildLogs)

	duration := time.Since(start).Milliseconds()

	if err != nil {
		sendResult(BuildResult{
			Success:    false,
			Error:      err.Error(),
			Logs:       logs.String(),
			Provenance: provenance,
			DurationMS: duration,
		})
		os.Exit(1)
	}

	// Success!
	log.Printf("=== Build Complete: %s ===", digest)
	provenance.Timestamp = time.Now()

	sendResult(BuildResult{
		Success:     true,
		ImageDigest: digest,
		Logs:        logs.String(),
		Provenance:  provenance,
		DurationMS:  duration,
	})
}

func loadConfig() (*BuildConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var config BuildConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func generateDockerfile(config *BuildConfig) (string, error) {
	switch {
	case strings.HasPrefix(config.Runtime, "nodejs"):
		return generateNodeDockerfile(config)
	case strings.HasPrefix(config.Runtime, "python"):
		return generatePythonDockerfile(config)
	default:
		return "", fmt.Errorf("unsupported runtime: %s", config.Runtime)
	}
}

func generateNodeDockerfile(config *BuildConfig) (string, error) {
	version := strings.TrimPrefix(config.Runtime, "nodejs")
	baseImage := config.BaseImageDigest
	if baseImage == "" {
		baseImage = fmt.Sprintf("node:%s-alpine", version)
	}

	// Detect lockfile
	lockfile := "package-lock.json"
	installCmd := "npm ci"
	if _, err := os.Stat(filepath.Join(config.SourcePath, "pnpm-lock.yaml")); err == nil {
		lockfile = "pnpm-lock.yaml"
		installCmd = "corepack enable && pnpm install --frozen-lockfile"
	} else if _, err := os.Stat(filepath.Join(config.SourcePath, "yarn.lock")); err == nil {
		lockfile = "yarn.lock"
		installCmd = "yarn install --frozen-lockfile"
	}

	return fmt.Sprintf(`FROM %s

WORKDIR /app

COPY package.json %s ./

RUN %s

COPY . .

CMD ["node", "index.js"]
`, baseImage, lockfile, installCmd), nil
}

func generatePythonDockerfile(config *BuildConfig) (string, error) {
	version := strings.TrimPrefix(config.Runtime, "python")
	baseImage := config.BaseImageDigest
	if baseImage == "" {
		baseImage = fmt.Sprintf("python:%s-slim", version)
	}

	reqPath := filepath.Join(config.SourcePath, "requirements.txt")
	hasHashes := false
	if data, err := os.ReadFile(reqPath); err == nil {
		hasHashes = strings.Contains(string(data), "--hash=")
	}

	var installCmd string
	if hasHashes {
		installCmd = "pip install --require-hashes --only-binary :all: -r requirements.txt"
	} else {
		installCmd = "pip install --no-cache-dir -r requirements.txt"
	}

	return fmt.Sprintf(`FROM %s

WORKDIR /app

COPY requirements.txt ./

RUN %s

COPY . .

CMD ["python", "main.py"]
`, baseImage, installCmd), nil
}

func runBuild(ctx context.Context, config *BuildConfig, logWriter io.Writer) (string, string, error) {
	var buildLogs bytes.Buffer

	// Build output reference
	outputRef := fmt.Sprintf("%s/builds/%s", config.RegistryURL, config.JobID)

	// Build arguments
	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + config.SourcePath,
		"--local", "dockerfile=" + config.SourcePath,
		"--output", fmt.Sprintf("type=image,name=%s,push=true", outputRef),
		"--metadata-file", "/tmp/build-metadata.json",
	}

	// Add cache if scope is set
	if config.CacheScope != "" {
		cacheRef := fmt.Sprintf("%s/cache/%s", config.RegistryURL, config.CacheScope)
		args = append(args, "--import-cache", fmt.Sprintf("type=registry,ref=%s", cacheRef))
		args = append(args, "--export-cache", fmt.Sprintf("type=registry,ref=%s,mode=max", cacheRef))
	}

	// Add secret mounts
	for _, secret := range config.Secrets {
		secretPath := fmt.Sprintf("/run/secrets/%s", secret.ID)
		args = append(args, "--secret", fmt.Sprintf("id=%s,src=%s", secret.ID, secretPath))
	}

	// Add build args
	for k, v := range config.BuildArgs {
		args = append(args, "--opt", fmt.Sprintf("build-arg:%s=%s", k, v))
	}

	log.Printf("Running: buildctl-daemonless.sh %s", strings.Join(args, " "))

	// Run buildctl-daemonless.sh
	cmd := exec.CommandContext(ctx, "buildctl-daemonless.sh", args...)
	cmd.Stdout = io.MultiWriter(logWriter, &buildLogs)
	cmd.Stderr = io.MultiWriter(logWriter, &buildLogs)
	cmd.Env = append(os.Environ(), "BUILDKITD_FLAGS=--oci-worker-no-process-sandbox")

	if err := cmd.Run(); err != nil {
		return "", buildLogs.String(), fmt.Errorf("buildctl failed: %w", err)
	}

	// Extract digest from metadata
	digest, err := extractDigest("/tmp/build-metadata.json")
	if err != nil {
		return "", buildLogs.String(), fmt.Errorf("extract digest: %w", err)
	}

	return digest, buildLogs.String(), nil
}

func extractDigest(metadataPath string) (string, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", err
	}

	var metadata struct {
		ContainerImageDigest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", err
	}

	if metadata.ContainerImageDigest == "" {
		return "", fmt.Errorf("no digest in metadata")
	}

	return metadata.ContainerImageDigest, nil
}

func computeProvenance(config *BuildConfig) BuildProvenance {
	prov := BuildProvenance{
		BaseImageDigest:  config.BaseImageDigest,
		LockfileHashes:   make(map[string]string),
		BuildkitVersion:  getBuildkitVersion(),
		ToolchainVersion: getToolchainVersion(config.Runtime),
	}

	// Hash lockfiles
	lockfiles := []string{
		"package-lock.json", "yarn.lock", "pnpm-lock.yaml",
		"requirements.txt", "poetry.lock", "Pipfile.lock",
	}
	for _, lf := range lockfiles {
		path := filepath.Join(config.SourcePath, lf)
		if hash, err := hashFile(path); err == nil {
			prov.LockfileHashes[lf] = hash
		}
	}

	// Hash source directory
	prov.SourceHash, _ = hashDirectory(config.SourcePath)

	return prov
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func hashDirectory(path string) (string, error) {
	h := sha256.New()
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		// Skip Dockerfile (generated) and hidden files
		name := filepath.Base(p)
		if name == "Dockerfile" || strings.HasPrefix(name, ".") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		relPath, _ := filepath.Rel(path, p)
		h.Write([]byte(relPath))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func getBuildkitVersion() string {
	cmd := exec.Command("buildctl", "--version")
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func getToolchainVersion(runtime string) string {
	switch {
	case strings.HasPrefix(runtime, "nodejs"):
		out, _ := exec.Command("node", "--version").Output()
		return strings.TrimSpace(string(out))
	case strings.HasPrefix(runtime, "python"):
		out, _ := exec.Command("python", "--version").Output()
		return strings.TrimSpace(string(out))
	}
	return "unknown"
}

func fetchSecrets(ctx context.Context, secrets []SecretRef) error {
	if len(secrets) == 0 {
		return nil
	}

	conn, err := dialVsock()
	if err != nil {
		return fmt.Errorf("dial vsock: %w", err)
	}
	defer conn.Close()

	// Request secrets
	secretIDs := make([]string, len(secrets))
	for i, s := range secrets {
		secretIDs[i] = s.ID
	}

	req := VsockMessage{
		Type: "get_secrets",
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}

	// Receive response
	var resp struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return err
	}

	// Write secrets to files
	if err := os.MkdirAll("/run/secrets", 0700); err != nil {
		return err
	}
	for _, s := range secrets {
		value, ok := resp.Secrets[s.ID]
		if !ok {
			return fmt.Errorf("secret not found: %s", s.ID)
		}
		path := fmt.Sprintf("/run/secrets/%s", s.ID)
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			return err
		}
	}

	return nil
}

func sendResult(result BuildResult) {
	conn, err := dialVsock()
	if err != nil {
		log.Printf("Failed to dial vsock: %v", err)
		return
	}
	defer conn.Close()

	msg := VsockMessage{
		Type:   "build_result",
		Result: &result,
	}

	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		log.Printf("Failed to send result: %v", err)
	}
}

func dialVsock() (net.Conn, error) {
	return vsock.Dial(hostCID, vsockPort, nil)
}

