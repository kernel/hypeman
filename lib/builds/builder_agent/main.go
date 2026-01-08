// Package main implements the builder agent that runs inside builder microVMs.
// It reads build configuration from the config disk, runs BuildKit to build
// the image, and reports results back to the host via vsock.
//
// Communication model:
// - Agent LISTENS on vsock port 5001
// - Host CONNECTS to the agent via the VM's vsock.sock file
// - This follows the Cloud Hypervisor vsock pattern (host initiates)
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"sync"
	"time"

	"github.com/mdlayher/vsock"
)

const (
	configPath = "/config/build.json"
	vsockPort  = 5001 // Build agent port (different from exec agent)
)

// BuildConfig matches the BuildConfig type from lib/builds/types.go
type BuildConfig struct {
	JobID           string            `json:"job_id"`
	BaseImageDigest string            `json:"base_image_digest,omitempty"`
	RegistryURL     string            `json:"registry_url"`
	RegistryToken   string            `json:"registry_token,omitempty"`
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
	BaseImageDigest string            `json:"base_image_digest"`
	SourceHash      string            `json:"source_hash"`
	LockfileHashes  map[string]string `json:"lockfile_hashes,omitempty"`
	BuildkitVersion string            `json:"buildkit_version,omitempty"`
	Timestamp       time.Time         `json:"timestamp"`
}

// VsockMessage is the envelope for vsock communication
type VsockMessage struct {
	Type      string            `json:"type"`
	Result    *BuildResult      `json:"result,omitempty"`
	Log       string            `json:"log,omitempty"`
	SecretIDs []string          `json:"secret_ids,omitempty"` // For secrets request to host
	Secrets   map[string]string `json:"secrets,omitempty"`    // For secrets response from host
}

// Global state for the result to send when host connects
var (
	buildResult     *BuildResult
	buildResultLock sync.Mutex
	buildDone       = make(chan struct{})

	// Secrets coordination
	buildConfig     *BuildConfig
	buildConfigLock sync.Mutex
	secretsReady    = make(chan struct{})
	secretsOnce     sync.Once
)

func main() {
	log.Println("=== Builder Agent Starting ===")

	// Start vsock listener first (so host can connect as soon as VM is ready)
	listener, err := startVsockListener()
	if err != nil {
		log.Fatalf("Failed to start vsock listener: %v", err)
	}
	defer listener.Close()
	log.Printf("Listening on vsock port %d", vsockPort)

	// Run the build in background
	go runBuildProcess()

	// Accept connections from host
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		go handleHostConnection(conn)
	}
}

// startVsockListener starts listening on vsock with retries (like exec-agent)
func startVsockListener() (*vsock.Listener, error) {
	var l *vsock.Listener
	var err error

	for i := 0; i < 10; i++ {
		l, err = vsock.Listen(vsockPort, nil)
		if err == nil {
			return l, nil
		}
		log.Printf("vsock listen attempt %d/10 failed: %v (retrying in 1s)", i+1, err)
		time.Sleep(1 * time.Second)
	}

	return nil, fmt.Errorf("failed to listen on vsock port %d after retries: %v", vsockPort, err)
}

// handleHostConnection handles a connection from the host
func handleHostConnection(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(reader)

	for {
		var msg VsockMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				return
			}
			log.Printf("Decode error: %v", err)
			return
		}

		switch msg.Type {
		case "host_ready":
			// Host is ready to handle requests
			// Request secrets if we have any configured
			if err := handleSecretsRequest(encoder, decoder); err != nil {
				log.Printf("Failed to fetch secrets: %v", err)
			}
			// Signal that secrets are ready (even if failed, build can proceed)
			secretsOnce.Do(func() {
				close(secretsReady)
			})

		case "get_result":
			// Host is asking for the build result
			// Wait for build to complete if not done yet
			<-buildDone

			buildResultLock.Lock()
			result := buildResult
			buildResultLock.Unlock()

			response := VsockMessage{
				Type:   "build_result",
				Result: result,
			}
			if err := encoder.Encode(response); err != nil {
				log.Printf("Failed to send result: %v", err)
			}
			return // Close connection after sending result

		case "get_status":
			// Host is checking if build is still running
			select {
			case <-buildDone:
				encoder.Encode(VsockMessage{Type: "status", Log: "completed"})
			default:
				encoder.Encode(VsockMessage{Type: "status", Log: "building"})
			}

		default:
			log.Printf("Unknown message type: %s", msg.Type)
		}
	}
}

// handleSecretsRequest requests secrets from the host and writes them to /run/secrets/
func handleSecretsRequest(encoder *json.Encoder, decoder *json.Decoder) error {
	// Wait for config to be loaded
	var config *BuildConfig
	for i := 0; i < 30; i++ {
		buildConfigLock.Lock()
		config = buildConfig
		buildConfigLock.Unlock()
		if config != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if config == nil {
		log.Printf("Config not loaded yet, skipping secrets")
		return nil
	}

	if len(config.Secrets) == 0 {
		log.Printf("No secrets configured")
		return nil
	}

	// Extract secret IDs
	secretIDs := make([]string, len(config.Secrets))
	for i, s := range config.Secrets {
		secretIDs[i] = s.ID
	}

	log.Printf("Requesting secrets: %v", secretIDs)

	// Send get_secrets request
	req := VsockMessage{
		Type:      "get_secrets",
		SecretIDs: secretIDs,
	}
	if err := encoder.Encode(req); err != nil {
		return fmt.Errorf("send get_secrets: %w", err)
	}

	// Wait for secrets_response
	var resp VsockMessage
	if err := decoder.Decode(&resp); err != nil {
		return fmt.Errorf("receive secrets_response: %w", err)
	}

	if resp.Type != "secrets_response" {
		return fmt.Errorf("unexpected response type: %s", resp.Type)
	}

	// Write secrets to /run/secrets/
	if err := os.MkdirAll("/run/secrets", 0700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}

	for id, value := range resp.Secrets {
		secretPath := fmt.Sprintf("/run/secrets/%s", id)
		if err := os.WriteFile(secretPath, []byte(value), 0600); err != nil {
			return fmt.Errorf("write secret %s: %w", id, err)
		}
		log.Printf("Wrote secret: %s", id)
	}

	log.Printf("Received %d secrets", len(resp.Secrets))
	return nil
}

// runBuildProcess runs the actual build and stores the result
func runBuildProcess() {
	start := time.Now()
	var logs bytes.Buffer
	logWriter := io.MultiWriter(os.Stdout, &logs)

	log.SetOutput(logWriter)

	defer func() {
		close(buildDone)
	}()

	// Load build config
	config, err := loadConfig()
	if err != nil {
		setResult(BuildResult{
			Success:    false,
			Error:      fmt.Sprintf("load config: %v", err),
			Logs:       logs.String(),
			DurationMS: time.Since(start).Milliseconds(),
		})
		return
	}
	log.Printf("Job: %s", config.JobID)

	// Store config globally so handleHostConnection can access it for secrets
	buildConfigLock.Lock()
	buildConfig = config
	buildConfigLock.Unlock()

	// Setup registry authentication before running the build
	if err := setupRegistryAuth(config.RegistryURL, config.RegistryToken); err != nil {
		setResult(BuildResult{
			Success:    false,
			Error:      fmt.Sprintf("setup registry auth: %v", err),
			Logs:       logs.String(),
			DurationMS: time.Since(start).Milliseconds(),
		})
		return
	}

	// Setup timeout context
	ctx := context.Background()
	if config.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(config.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	// Wait for secrets if any are configured
	if len(config.Secrets) > 0 {
		log.Printf("Waiting for secrets from host...")
		select {
		case <-secretsReady:
			log.Printf("Secrets ready, proceeding with build")
		case <-time.After(30 * time.Second):
			log.Printf("Warning: Timeout waiting for secrets, proceeding anyway")
			// Signal secrets ready to avoid blocking other goroutines
			secretsOnce.Do(func() {
				close(secretsReady)
			})
		case <-ctx.Done():
			setResult(BuildResult{
				Success:    false,
				Error:      "build timeout while waiting for secrets",
				Logs:       logs.String(),
				DurationMS: time.Since(start).Milliseconds(),
			})
			return
		}
	}

	// Ensure Dockerfile exists (either in source or provided via config)
	dockerfilePath := filepath.Join(config.SourcePath, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		// Check if Dockerfile was provided in config
		if config.Dockerfile == "" {
			setResult(BuildResult{
				Success:    false,
				Error:      "Dockerfile required: provide dockerfile parameter or include Dockerfile in source tarball",
				Logs:       logs.String(),
				DurationMS: time.Since(start).Milliseconds(),
			})
			return
		}
		// Write provided Dockerfile to source directory
		if err := os.WriteFile(dockerfilePath, []byte(config.Dockerfile), 0644); err != nil {
			setResult(BuildResult{
				Success:    false,
				Error:      fmt.Sprintf("write dockerfile: %v", err),
				Logs:       logs.String(),
				DurationMS: time.Since(start).Milliseconds(),
			})
			return
		}
		log.Println("Using Dockerfile from config")
	} else {
		log.Println("Using Dockerfile from source")
	}

	// Compute provenance
	provenance := computeProvenance(config)

	// Run the build
	log.Println("=== Starting Build ===")
	digest, buildLogs, err := runBuild(ctx, config, logWriter)
	logs.WriteString(buildLogs)

	duration := time.Since(start).Milliseconds()

	if err != nil {
		setResult(BuildResult{
			Success:    false,
			Error:      err.Error(),
			Logs:       logs.String(),
			Provenance: provenance,
			DurationMS: duration,
		})
		return
	}

	// Success!
	log.Printf("=== Build Complete: %s ===", digest)
	provenance.Timestamp = time.Now()

	setResult(BuildResult{
		Success:     true,
		ImageDigest: digest,
		Logs:        logs.String(),
		Provenance:  provenance,
		DurationMS:  duration,
	})
}

// setResult stores the build result for the host to retrieve
func setResult(result BuildResult) {
	buildResultLock.Lock()
	defer buildResultLock.Unlock()
	buildResult = &result
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

// setupRegistryAuth creates a Docker config.json with the registry token for authentication.
// BuildKit uses this file to authenticate when pushing images.
func setupRegistryAuth(registryURL, token string) error {
	if token == "" {
		log.Println("No registry token provided, skipping auth setup")
		return nil
	}

	// Docker config format expects base64-encoded "username:password" or just the token
	// For bearer tokens, we use the token directly as the "auth" value
	// Format: base64(token + ":") - empty password
	authValue := base64.StdEncoding.EncodeToString([]byte(token + ":"))

	// Create the Docker config structure
	dockerConfig := map[string]interface{}{
		"auths": map[string]interface{}{
			registryURL: map[string]string{
				"auth": authValue,
			},
		},
	}

	configData, err := json.MarshalIndent(dockerConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal docker config: %w", err)
	}

	// Ensure ~/.docker directory exists
	dockerDir := "/home/builder/.docker"
	if err := os.MkdirAll(dockerDir, 0700); err != nil {
		return fmt.Errorf("create docker config dir: %w", err)
	}

	// Write config.json
	configPath := filepath.Join(dockerDir, "config.json")
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		return fmt.Errorf("write docker config: %w", err)
	}

	log.Printf("Registry auth configured for %s", registryURL)
	return nil
}

func runBuild(ctx context.Context, config *BuildConfig, logWriter io.Writer) (string, string, error) {
	var buildLogs bytes.Buffer

	// Build output reference
	outputRef := fmt.Sprintf("%s/builds/%s", config.RegistryURL, config.JobID)

	// Build arguments
	// Use registry.insecure=true for internal HTTP registries
	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + config.SourcePath,
		"--local", "dockerfile=" + config.SourcePath,
		"--output", fmt.Sprintf("type=image,name=%s,push=true,registry.insecure=true,oci-mediatypes=true", outputRef),
		"--metadata-file", "/tmp/build-metadata.json",
	}

	// Add cache if scope is set
	if config.CacheScope != "" {
		cacheRef := fmt.Sprintf("%s/cache/%s", config.RegistryURL, config.CacheScope)
		args = append(args, "--import-cache", fmt.Sprintf("type=registry,ref=%s,registry.insecure=true", cacheRef))
		args = append(args, "--export-cache", fmt.Sprintf("type=registry,ref=%s,mode=max,registry.insecure=true", cacheRef))
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
	// Use BUILDKITD_FLAGS from environment (set in Dockerfile) or empty for default
	cmd.Env = os.Environ()

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
		BaseImageDigest: config.BaseImageDigest,
		LockfileHashes:  make(map[string]string),
		BuildkitVersion: getBuildkitVersion(),
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
