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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	controlSocketFile = "control.sock"
	pagerPIDFile      = "pager.pid"
	pagerLogFile      = "pager.log"
	sessionsDir       = "sessions"
)

type Supervisor struct {
	dataDir       string
	versionKey    string
	cacheMaxBytes int64
	controlSocket string
	executable    string

	mu      sync.Mutex
	clients map[string]*http.Client
}

func NewSupervisor(ctx context.Context, dataDir string, cacheMaxBytes int64) (*Supervisor, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	versionKey, err := executableVersionKey(exe)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{
		dataDir:       dataDir,
		versionKey:    versionKey,
		cacheMaxBytes: normalizeCacheMaxBytes(cacheMaxBytes),
		controlSocket: pagerControlSocket(dataDir, versionKey),
		executable:    exe,
		clients:       make(map[string]*http.Client),
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

	dir := pagerVersionDir(s.dataDir, s.versionKey)
	if err := os.MkdirAll(filepath.Join(dir, sessionsDir), 0755); err != nil {
		return fmt.Errorf("create uffd pager directory: %w", err)
	}
	_ = os.Remove(s.controlSocket)

	logFile, err := os.OpenFile(filepath.Join(dir, pagerLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open uffd pager log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(
		s.executable,
		"--internal-uffd-pager",
		"--data-dir", s.dataDir,
		"--version-key", s.versionKey,
		"--cache-max-bytes", strconv.FormatInt(s.cacheMaxBytes, 10),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start uffd pager: %w", err)
	}
	_ = os.WriteFile(filepath.Join(dir, pagerPIDFile), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0644)
	_ = cmd.Process.Release()

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

func executableVersionKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("hash executable %q: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12], nil
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
