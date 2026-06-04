//go:build linux

package uffdpager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	controlSocketFile = "control.sock"
	pagerPIDFile      = "pager.pid"
	sessionsDir       = "sessions"

	systemdUnitTemplate  = "hypeman-uffd@.service"
	systemdRuntimeEnvDir = "/run/hypeman/uffd"
)

var invalidSystemdInstancePartChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Supervisor struct {
	dataDir            string
	versionKey         string
	systemdInstanceKey string
	cacheMaxBytes      int64
	controlSocket      string

	mu      sync.Mutex
	clients map[string]*http.Client
}

func NewSupervisor(ctx context.Context, dataDir string, cacheMaxBytes int64) (*Supervisor, error) {
	versionKey := Version()
	if versionKey == "" {
		return nil, fmt.Errorf("uffd pager version is empty")
	}
	s := &Supervisor{
		dataDir:            dataDir,
		versionKey:         versionKey,
		systemdInstanceKey: systemdInstanceKey(versionKey, dataDir),
		cacheMaxBytes:      normalizeCacheMaxBytes(cacheMaxBytes),
		controlSocket:      pagerControlSocket(dataDir, versionKey),
		clients:            make(map[string]*http.Client),
	}
	if err := s.ensureRunning(ctx); err != nil {
		return nil, err
	}
	s.drainOlderPagers(ctx)
	return s, nil
}

func (s *Supervisor) VersionKey() string {
	if s == nil {
		return ""
	}
	return s.versionKey
}

func (s *Supervisor) CreateSession(ctx context.Context, req CreateSessionRequest) (*CreateSessionResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("uffd pager supervisor is nil")
	}
	if strings.TrimSpace(req.SessionID) == "" {
		req.SessionID = req.InstanceID
	}
	var resp CreateSessionResponse
	if err := s.doJSON(ctx, s.versionKey, http.MethodPost, "/sessions", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (s *Supervisor) CloseSession(ctx context.Context, sessionID string) error {
	if s == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	return s.CloseSessionVersion(ctx, s.versionKey, sessionID)
}

func (s *Supervisor) CloseSessionVersion(ctx context.Context, versionKey, sessionID string) error {
	if s == nil || strings.TrimSpace(versionKey) == "" || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	path := "/sessions/" + urlPathEscape(sessionID) + "/close"
	return s.doJSON(ctx, versionKey, http.MethodPost, path, nil, nil)
}

func (s *Supervisor) Stats(ctx context.Context) (*Stats, error) {
	if s == nil {
		return nil, fmt.Errorf("uffd pager supervisor is nil")
	}
	var stats Stats
	if err := s.doJSON(ctx, s.versionKey, http.MethodGet, "/stats", nil, &stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

func (s *Supervisor) HealthVersion(ctx context.Context, versionKey string) (*HealthResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("uffd pager supervisor is nil")
	}
	var health HealthResponse
	if err := s.doJSON(ctx, versionKey, http.MethodGet, "/health", nil, &health); err != nil {
		return nil, err
	}
	return &health, nil
}

func (s *Supervisor) ensureRunning(ctx context.Context) error {
	if s.isHealthy(ctx, s.versionKey) {
		return nil
	}
	if !s.systemdTemplateInstalled(ctx) {
		return fmt.Errorf("uffd pager systemd unit template %s is not installed", systemdUnitTemplate)
	}
	if err := s.ensureRunningSystemd(ctx); err != nil {
		return err
	}
	return s.waitHealthy(ctx)
}

func (s *Supervisor) ensureRunningSystemd(ctx context.Context) error {
	dir := pagerVersionDir(s.dataDir, s.versionKey)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create uffd pager directory: %w", err)
	}
	if err := os.MkdirAll(systemdRuntimeEnvDir, 0755); err != nil {
		return fmt.Errorf("create uffd pager systemd environment directory: %w", err)
	}
	env := s.systemdEnvironment()
	if err := os.WriteFile(systemdEnvFile(s.systemdInstanceKey), []byte(env), 0644); err != nil {
		return fmt.Errorf("write uffd pager systemd environment: %w", err)
	}
	unit := systemdUnitName(s.systemdInstanceKey)
	cmd := exec.CommandContext(ctx, "systemctl", "start", unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start uffd pager systemd unit %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Supervisor) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if s.isHealthy(ctx, s.versionKey) {
			return nil
		}
		lastErr = ctx.Err()
		if lastErr != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("wait for uffd pager health: %w", lastErr)
	}
	return fmt.Errorf("uffd pager did not become healthy at %s", s.controlSocket)
}

func (s *Supervisor) systemdTemplateInstalled(ctx context.Context) bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, "systemctl", "cat", systemdUnitTemplate)
	return cmd.Run() == nil
}

func (s *Supervisor) systemdEnvironment() string {
	var b strings.Builder
	b.WriteString(systemdEnvLine("HYPEMAN_UFFD_CACHE_MAX_BYTES", fmt.Sprintf("%d", s.cacheMaxBytes)))
	b.WriteString(systemdEnvLine("HYPEMAN_UFFD_DATA_DIR", s.dataDir))
	b.WriteString(systemdEnvLine("HYPEMAN_UFFD_VERSION_KEY", s.versionKey))
	if binary := strings.TrimSpace(os.Getenv("HYPEMAN_UFFD_PAGER_BINARY")); binary != "" {
		b.WriteString(systemdEnvLine("HYPEMAN_UFFD_BINARY", binary))
	}
	return b.String()
}

func (s *Supervisor) isHealthy(ctx context.Context, versionKey string) bool {
	var health HealthResponse
	return s.doJSON(ctx, versionKey, http.MethodGet, "/health", nil, &health) == nil
}

func (s *Supervisor) drainOlderPagers(ctx context.Context) {
	root := filepath.Join(s.dataDir, "uffd")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == s.versionKey {
			continue
		}
		_ = s.doJSON(ctx, entry.Name(), http.MethodPost, "/drain", nil, nil)
	}
}

func (s *Supervisor) doJSON(ctx context.Context, versionKey, method, path string, body any, out any) error {
	client := s.clientForVersion(versionKey)
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal uffd pager request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("uffd pager %s %s returned %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode uffd pager response: %w", err)
	}
	return nil
}

func (s *Supervisor) clientForVersion(versionKey string) *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if client := s.clients[versionKey]; client != nil {
		return client
	}
	socketPath := pagerControlSocket(s.dataDir, versionKey)
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 10 * time.Second,
	}
	s.clients[versionKey] = client
	return client
}

func pagerVersionDir(dataDir, versionKey string) string {
	return filepath.Join(dataDir, "uffd", versionKey)
}

func pagerControlSocket(dataDir, versionKey string) string {
	return filepath.Join(pagerVersionDir(dataDir, versionKey), controlSocketFile)
}

func urlPathEscape(value string) string {
	replacer := strings.NewReplacer("%", "%25", "/", "%2F", "?", "%3F", "#", "%23")
	return replacer.Replace(value)
}

func systemdInstanceKey(versionKey, dataDir string) string {
	prefix := sanitizeSystemdInstancePart(os.Getenv("HYPEMAN_UFFD_SYSTEMD_INSTANCE_PREFIX"))
	if prefix == "" {
		return versionKey
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dataDir)))
	return prefix + "-" + versionKey + "-" + hex.EncodeToString(sum[:4])
}

func sanitizeSystemdInstancePart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return strings.Trim(invalidSystemdInstancePartChars.ReplaceAllString(value, "-"), "-")
}

func systemdEnvFile(instanceKey string) string {
	return filepath.Join(systemdRuntimeEnvDir, instanceKey+".env")
}

func systemdEnvLine(key, value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return key + `="` + escaped + `"` + "\n"
}

func systemdUnitName(instanceKey string) string {
	return "hypeman-uffd@" + instanceKey + ".service"
}
