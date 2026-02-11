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
	JobID            string            `json:"job_id"`
	BaseImageDigest  string            `json:"base_image_digest,omitempty"`
	RegistryURL      string            `json:"registry_url"`
	RegistryToken    string            `json:"registry_token,omitempty"`
	RegistryInsecure bool              `json:"registry_insecure,omitempty"`
	RegistryCACert   string            `json:"registry_ca_cert,omitempty"`
	CacheScope       string            `json:"cache_scope,omitempty"`
	SourcePath       string            `json:"source_path"`
	Dockerfile       string            `json:"dockerfile,omitempty"`
	BuildArgs        map[string]string `json:"build_args,omitempty"`
	Secrets          []SecretRef       `json:"secrets,omitempty"`
	TimeoutSeconds   int               `json:"timeout_seconds"`
	NetworkMode      string            `json:"network_mode"`
	IsAdminBuild     bool              `json:"is_admin_build,omitempty"`
	GlobalCacheKey   string            `json:"global_cache_key,omitempty"`
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

	// Encoder lock protects concurrent access to json.Encoder
	// (the goroutine sending build_result and the main loop handling get_status)
	encoderLock sync.Mutex

	// Log streaming channel - logs are sent here and forwarded to host via vsock
	logChan     = make(chan string, 1000)
	logChanOnce sync.Once
)

// streamingLogWriter writes log lines to a channel for streaming to the host.
// It also writes to a buffer to include all logs in the final result.
type streamingLogWriter struct {
	buffer   *bytes.Buffer
	mu       sync.Mutex
	closed   bool
	closedMu sync.RWMutex
}

func newStreamingLogWriter() *streamingLogWriter {
	return &streamingLogWriter{
		buffer: &bytes.Buffer{},
	}
}

func (w *streamingLogWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	w.buffer.Write(p)
	w.mu.Unlock()

	// Hold RLock through the send to prevent markClosed()+close(logChan)
	// from racing between the check and the channel send.
	w.closedMu.RLock()
	if !w.closed {
		line := string(p)
		select {
		case logChan <- line:
		default:
			// Channel full, drop the log line for streaming but it's still in buffer
		}
	}
	w.closedMu.RUnlock()

	// Also write to stdout for local debugging
	os.Stdout.Write(p)

	return len(p), nil
}

func (w *streamingLogWriter) markClosed() {
	w.closedMu.Lock()
	w.closed = true
	w.closedMu.Unlock()
}

func (w *streamingLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func main() {
	log.Println("=== Builder Agent Starting ===")

	// Start guest-agent for exec/debugging support (runs in background)
	startGuestAgent()

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

// startGuestAgent starts the guest-agent binary for exec/debugging support.
// The guest-agent listens on vsock port 2222 and provides exec capability
// so operators can debug failed builds.
func startGuestAgent() {
	guestAgentPath := "/usr/bin/guest-agent"

	// Check if guest-agent exists
	if _, err := os.Stat(guestAgentPath); os.IsNotExist(err) {
		log.Printf("guest-agent not found at %s (exec disabled)", guestAgentPath)
		return
	}

	// Start guest-agent in background
	cmd := exec.Command(guestAgentPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start guest-agent: %v", err)
		return
	}

	log.Printf("Started guest-agent (PID %d) for exec support", cmd.Process.Pid)

	// Let the process run in background - don't wait for it
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("guest-agent exited: %v", err)
		}
	}()
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

			// Start streaming logs to host
			logsDone := make(chan struct{})
			go func() {
				defer close(logsDone)
				for logLine := range logChan {
					encoderLock.Lock()
					err := encoder.Encode(VsockMessage{Type: "log", Log: logLine})
					encoderLock.Unlock()
					if err != nil {
						// Connection closed, stop streaming
						return
					}
				}
			}()

			// Wait for build to complete and send result to host
			go func() {
				<-buildDone
				// Wait for all buffered log messages to be sent before sending the result.
				// This prevents the host from receiving build_result before all logs,
				// which would cause it to close the connection and lose remaining logs.
				<-logsDone

				buildResultLock.Lock()
				result := buildResult
				buildResultLock.Unlock()

				log.Printf("Build completed, sending result to host")
				encoderLock.Lock()
				err := encoder.Encode(VsockMessage{Type: "build_result", Result: result})
				encoderLock.Unlock()
				if err != nil {
					log.Printf("Failed to send build result: %v", err)
				}
			}()

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
			encoderLock.Lock()
			err := encoder.Encode(response)
			encoderLock.Unlock()
			if err != nil {
				log.Printf("Failed to send result: %v", err)
			}
			return // Close connection after sending result

		case "get_status":
			// Host is checking if build is still running
			encoderLock.Lock()
			select {
			case <-buildDone:
				encoder.Encode(VsockMessage{Type: "status", Log: "completed"})
			default:
				encoder.Encode(VsockMessage{Type: "status", Log: "building"})
			}
			encoderLock.Unlock()

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
	encoderLock.Lock()
	err := encoder.Encode(req)
	encoderLock.Unlock()
	if err != nil {
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
	logWriter := newStreamingLogWriter()

	log.SetOutput(logWriter)

	defer func() {
		// Mark writer as closed first to prevent writes to closed channel
		logWriter.markClosed()
		// Close log channel so streaming goroutine terminates
		logChanOnce.Do(func() {
			close(logChan)
		})
		close(buildDone)
	}()

	// Load build config
	config, err := loadConfig()
	if err != nil {
		setResult(BuildResult{
			Success:    false,
			Error:      fmt.Sprintf("load config: %v", err),
			Logs:       logWriter.String(),
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
	if err := setupRegistryAuth(config); err != nil {
		setResult(BuildResult{
			Success:    false,
			Error:      fmt.Sprintf("setup registry auth: %v", err),
			Logs:       logWriter.String(),
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
				Logs:       logWriter.String(),
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
				Logs:       logWriter.String(),
				DurationMS: time.Since(start).Milliseconds(),
			})
			return
		}
		// Write provided Dockerfile to source directory
		if err := os.WriteFile(dockerfilePath, []byte(config.Dockerfile), 0644); err != nil {
			setResult(BuildResult{
				Success:    false,
				Error:      fmt.Sprintf("write dockerfile: %v", err),
				Logs:       logWriter.String(),
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
	digest, _, err := runBuild(ctx, config, logWriter)

	duration := time.Since(start).Milliseconds()

	if err != nil {
		setResult(BuildResult{
			Success:    false,
			Error:      err.Error(),
			Logs:       logWriter.String(),
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
		Logs:        logWriter.String(),
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

// setupRegistryAuth creates a Docker config.json with the registry token for authentication,
// and a buildkitd.toml for TLS configuration.
// BuildKit uses these files to authenticate and configure TLS when pushing images.
func setupRegistryAuth(config *BuildConfig) error {
	// Parse registry host (strip any scheme prefix for backwards compatibility)
	registryHost := config.RegistryURL
	if strings.HasPrefix(registryHost, "https://") {
		registryHost = strings.TrimPrefix(registryHost, "https://")
	} else if strings.HasPrefix(registryHost, "http://") {
		registryHost = strings.TrimPrefix(registryHost, "http://")
	}

	token := config.RegistryToken

	if token == "" {
		log.Println("No registry token provided, skipping auth setup")
		return nil
	}

	// Docker config format expects base64-encoded "username:password" or just the token
	// For bearer tokens, we use the token directly as the "auth" value
	// Format: base64(token + ":") - empty password
	authValue := base64.StdEncoding.EncodeToString([]byte(token + ":"))

	// Create the Docker config structure
	// Note: Docker config uses host without scheme (e.g., "10.102.0.1:8443")
	// We use both auth (Basic) and identitytoken (JWT) to support different BuildKit versions
	dockerConfig := map[string]interface{}{
		"auths": map[string]interface{}{
			registryHost: map[string]string{
				"auth":          authValue,      // Basic auth: base64(jwt:)
				"identitytoken": token,          // JWT directly for OAuth2-style auth
			},
		},
		"credsStore":  "",
		"credHelpers": map[string]string{},
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

	log.Printf("Docker config created for registry %s (auth length: %d)", registryHost, len(authValue))

	// Also write to /root/.docker for rootless buildkit that may run as root
	rootDockerDir := "/root/.docker"
	if err := os.MkdirAll(rootDockerDir, 0700); err == nil {
		rootConfigPath := filepath.Join(rootDockerDir, "config.json")
		if err := os.WriteFile(rootConfigPath, configData, 0600); err != nil {
			log.Printf("Warning: failed to write root docker config: %v", err)
		} else {
			log.Printf("Registry auth configured at %s", rootConfigPath)
		}
	}

	log.Printf("Registry auth configured at %s", configPath)

	// Setup buildkitd.toml for TLS configuration
	if err := setupBuildkitdConfig(config); err != nil {
		return fmt.Errorf("setup buildkitd config: %w", err)
	}

	return nil
}

// setupBuildkitdConfig creates a buildkitd.toml configuration file for registry TLS settings.
// This configures BuildKit's TLS verification behavior for the registry.
func setupBuildkitdConfig(config *BuildConfig) error {
	// Parse registry host from URL (strip any scheme prefix for backwards compatibility)
	registryHost := config.RegistryURL
	if strings.HasPrefix(registryHost, "https://") {
		registryHost = strings.TrimPrefix(registryHost, "https://")
	} else if strings.HasPrefix(registryHost, "http://") {
		registryHost = strings.TrimPrefix(registryHost, "http://")
	}

	// Determine protocol:
	// - RegistryInsecure=true means use HTTP (plaintext)
	// - RegistryInsecure=false (default) means use HTTPS
	isHTTPS := !config.RegistryInsecure
	hasCA := config.RegistryCACert != ""

	log.Printf("BuildKit config for registry %s (https=%v, insecure=%v, hasCA=%v)",
		registryHost, isHTTPS, config.RegistryInsecure, hasCA)

	// Write CA certificate if provided
	caCertPath := ""
	if hasCA {
		caCertPath = "/home/builder/.config/buildkit/registry-ca.crt"
		certDir := filepath.Dir(caCertPath)
		if err := os.MkdirAll(certDir, 0755); err != nil {
			return fmt.Errorf("create cert dir: %w", err)
		}
		if err := os.WriteFile(caCertPath, []byte(config.RegistryCACert), 0644); err != nil {
			return fmt.Errorf("write CA cert: %w", err)
		}
		log.Printf("Registry CA certificate written to %s", caCertPath)

		// Also install CA cert system-wide so BuildKit's HTTP client trusts it
		// (needed for the /v2/token endpoint which uses Go's default HTTP client)
		systemCADir := "/usr/local/share/ca-certificates"
		if err := os.MkdirAll(systemCADir, 0755); err != nil {
			log.Printf("Warning: failed to create system CA dir: %v", err)
		} else {
			systemCAPath := filepath.Join(systemCADir, "hypeman-registry.crt")
			if err := os.WriteFile(systemCAPath, []byte(config.RegistryCACert), 0644); err != nil {
				log.Printf("Warning: failed to write system CA cert: %v", err)
			} else {
				// Run update-ca-certificates to add to system trust store
				cmd := exec.Command("update-ca-certificates")
				if output, err := cmd.CombinedOutput(); err != nil {
					log.Printf("Warning: update-ca-certificates failed: %v: %s", err, output)
				} else {
					log.Printf("Installed CA cert system-wide")
				}
			}
		}
	}

	// Build the buildkitd.toml content
	var tomlContent strings.Builder
	tomlContent.WriteString("# BuildKit daemon configuration\n")
	tomlContent.WriteString("# Generated by builder-agent for registry TLS\n\n")

	// Registry configuration section
	tomlContent.WriteString(fmt.Sprintf("[registry.\"%s\"]\n", registryHost))

	if !isHTTPS {
		// HTTP registry - mark as insecure (plaintext)
		tomlContent.WriteString("  http = true\n")
		tomlContent.WriteString("  insecure = true\n")
	} else if config.RegistryInsecure {
		// HTTPS but skip TLS verification
		tomlContent.WriteString("  insecure = true\n")
	} else if hasCA {
		// HTTPS with custom CA
		tomlContent.WriteString(fmt.Sprintf("  ca = [\"%s\"]\n", caCertPath))
	}
	// If HTTPS without insecure and without CA, use system CA (no config needed)

	// Ensure config directory exists
	buildkitDir := "/home/builder/.config/buildkit"
	if err := os.MkdirAll(buildkitDir, 0755); err != nil {
		return fmt.Errorf("create buildkit config dir: %w", err)
	}

	// Write buildkitd.toml
	tomlPath := filepath.Join(buildkitDir, "buildkitd.toml")
	if err := os.WriteFile(tomlPath, []byte(tomlContent.String()), 0644); err != nil {
		return fmt.Errorf("write buildkitd.toml: %w", err)
	}

	log.Printf("BuildKit config written to %s for registry %s (https=%v, insecure=%v, hasCA=%v)",
		tomlPath, registryHost, isHTTPS, config.RegistryInsecure, hasCA)

	return nil
}

func runBuild(ctx context.Context, config *BuildConfig, logWriter io.Writer) (string, string, error) {
	var buildLogs bytes.Buffer

	// Parse registry host (strip any scheme prefix for backwards compatibility)
	registryHost := config.RegistryURL
	if strings.HasPrefix(registryHost, "https://") {
		registryHost = strings.TrimPrefix(registryHost, "https://")
	} else if strings.HasPrefix(registryHost, "http://") {
		registryHost = strings.TrimPrefix(registryHost, "http://")
	}

	// Build output reference (use host without scheme)
	outputRef := fmt.Sprintf("%s/builds/%s", registryHost, config.JobID)

	// Determine protocol:
	// - RegistryInsecure=true means use HTTP (plaintext), needs registry.insecure=true in buildctl
	// - RegistryInsecure=false (default) means use HTTPS, TLS config comes from buildkitd.toml
	useInsecureFlag := config.RegistryInsecure

	// Build arguments
	var outputOpts string
	if useInsecureFlag {
		outputOpts = fmt.Sprintf("type=image,name=%s,push=true,registry.insecure=true,oci-mediatypes=true,compression=zstd,force-compression=true", outputRef)
		log.Printf("Using HTTP registry (insecure mode): %s", registryHost)
	} else {
		outputOpts = fmt.Sprintf("type=image,name=%s,push=true,oci-mediatypes=true,compression=zstd,force-compression=true", outputRef)
		log.Printf("Using HTTPS registry (secure mode): %s", registryHost)
	}

	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + config.SourcePath,
		"--local", "dockerfile=" + config.SourcePath,
		"--output", outputOpts,
		"--metadata-file", "/tmp/build-metadata.json",
	}

	// Two-tier cache implementation:
	// 1. Import from global cache (if runtime specified) - always read-only for regular builds
	// 2. Import from tenant cache (if cache scope specified)
	// 3. Export to appropriate target based on build type

	// Import from global cache (read-only for regular builds, read-write for admin builds)
	if config.GlobalCacheKey != "" {
		globalCacheRef := fmt.Sprintf("%s/cache/global/%s", registryHost, config.GlobalCacheKey)
		cacheOpts := "type=registry,ref=" + globalCacheRef
		if useInsecureFlag {
			cacheOpts += ",registry.insecure=true"
		}
		args = append(args, "--import-cache", cacheOpts)
		log.Printf("Importing from global cache: %s", globalCacheRef)
	}

	// For regular builds, also import from tenant cache if scope is set
	if !config.IsAdminBuild && config.CacheScope != "" {
		tenantCacheRef := fmt.Sprintf("%s/cache/%s", registryHost, config.CacheScope)
		cacheOpts := "type=registry,ref=" + tenantCacheRef
		if useInsecureFlag {
			cacheOpts += ",registry.insecure=true"
		}
		args = append(args, "--import-cache", cacheOpts)
		log.Printf("Importing from tenant cache: %s", tenantCacheRef)
	}

	// Export cache based on build type
	// Note: image-manifest=true ensures layer blobs are stored in the registry cache image
	// rather than as references to external registries (e.g., docker.io). This is critical
	// for cache hits in ephemeral BuildKit instances that don't have local layer storage.
	if config.IsAdminBuild {
		// Admin build: export to global cache
		if config.GlobalCacheKey != "" {
			globalCacheRef := fmt.Sprintf("%s/cache/global/%s", registryHost, config.GlobalCacheKey)
			cacheOpts := "type=registry,ref=" + globalCacheRef + ",mode=max,image-manifest=true,oci-mediatypes=true"
			if useInsecureFlag {
				cacheOpts += ",registry.insecure=true"
			}
			args = append(args, "--export-cache", cacheOpts)
			log.Printf("Exporting to global cache (admin build): %s", globalCacheRef)
		}
	} else {
		// Regular build: export to tenant cache
		if config.CacheScope != "" {
			tenantCacheRef := fmt.Sprintf("%s/cache/%s", registryHost, config.CacheScope)
			cacheOpts := "type=registry,ref=" + tenantCacheRef + ",mode=max,image-manifest=true,oci-mediatypes=true"
			if useInsecureFlag {
				cacheOpts += ",registry.insecure=true"
			}
			args = append(args, "--export-cache", cacheOpts)
			log.Printf("Exporting to tenant cache: %s", tenantCacheRef)
		}
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

	// Set buildkitd config path
	buildkitdConfig := "/home/builder/.config/buildkit/buildkitd.toml"
	log.Printf("Using buildkitd config: %s", buildkitdConfig)

	// Mount a tmpfs for BuildKit's data directory.
	// The VM rootfs is an overlayfs (read-only ext4 + writable ext4 upper layer).
	// BuildKit's native overlayfs snapshotter creates char device 0:0 for whiteout
	// markers, but mknod(char 0:0) fails on an overlayfs mount because the kernel
	// treats it as an overlayfs whiteout rather than a regular device node.
	// Using tmpfs avoids this nested-overlayfs conflict.
	buildkitRoot := "/var/lib/buildkit"
	if err := os.MkdirAll(buildkitRoot, 0755); err != nil {
		return "", "", fmt.Errorf("create buildkit root dir: %w", err)
	}
	mountCmd := exec.Command("mount", "-t", "tmpfs", "-o", "size=3G", "tmpfs", buildkitRoot)
	if output, err := mountCmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("mount tmpfs at %s (required for native overlayfs snapshotter): %v: %s", buildkitRoot, err, output)
	}
	log.Printf("Mounted tmpfs at %s for BuildKit snapshotter", buildkitRoot)

	log.Printf("Running: buildctl-daemonless.sh %s", strings.Join(args, " "))

	// Run buildctl-daemonless.sh
	// buildctl writes progress (#1, #2, etc.) to stderr and a duplicate summary to stdout.
	// Only pipe stderr to logWriter to avoid doubled output in build logs.
	cmd := exec.CommandContext(ctx, "buildctl-daemonless.sh", args...)
	cmd.Stdout = &buildLogs
	cmd.Stderr = io.MultiWriter(logWriter, &buildLogs)
	// Set environment:
	// - HOME and DOCKER_CONFIG: ensures buildctl finds the auth config at /root/.docker/config.json
	// - BUILDKITD_FLAGS: tells buildkitd to use our custom config for registry TLS settings
	//   and to use native overlayfs snapshotter with a tmpfs-backed root directory
	// Filter out existing values to avoid duplicates (first value wins in shell)
	env := make([]string, 0, len(os.Environ())+3)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "DOCKER_CONFIG=") &&
		   !strings.HasPrefix(e, "BUILDKITD_FLAGS=") &&
		   !strings.HasPrefix(e, "HOME=") {
			env = append(env, e)
		}
	}
	env = append(env, "HOME=/root")
	env = append(env, "DOCKER_CONFIG=/root/.docker")
	env = append(env, fmt.Sprintf("BUILDKITD_FLAGS=--config=%s --oci-worker-snapshotter=overlayfs --root=%s", buildkitdConfig, buildkitRoot))
	cmd.Env = env

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
