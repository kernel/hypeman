// publish-macos-base is an offline administrator tool, not an HTTP filesystem API.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/images"
)

type audit struct {
	Version            int    `json:"version"`
	Status             string `json:"status"`
	OSVersion          string `json:"os_version"`
	OSBuild            string `json:"os_build"`
	Architecture       string `json:"architecture"`
	AgentSHA256        string `json:"agent_sha256"`
	PlistSHA256        string `json:"plist_sha256"`
	ToolchainSHA256    string `json:"toolchain_sha256"`
	ToolchainVersion   string `json:"toolchain_version"`
	HomeBaselineSHA256 string `json:"home_baseline_sha256"`
	AuditScriptSHA256  string `json:"audit_script_sha256"`
}

type receipt struct {
	Version       int               `json:"version"`
	Reference     string            `json:"reference"`
	Digest        string            `json:"digest"`
	Audit         audit             `json:"audit"`
	AuditSHA256   string            `json:"audit_sha256"`
	PayloadSHA256 map[string]string `json:"payload_sha256"`
	PublishedAt   time.Time         `json:"published_at"`
}

func main() {
	source := flag.String("source", "", "exclusive stopped macvm bundle")
	auditPath := flag.String("audit", "", "guest audit JSON from final GuestService exec")
	registry := flag.String("registry", "", "configured internal registry URL")
	key := flag.String("registry-secret-file", "", "private file containing internal registry signing secret")
	id := flag.String("id", "", "unique lowercase base publication ID (base-...) ")
	out := flag.String("output", "", "new receipt file (must not exist)")
	stopped := flag.Bool("stopped", false, "attest graceful shutdown and VMM exit; no restart during export")
	clean := flag.Bool("clean-install-reviewed", false, "attest fresh installation, no real credentials ever, final audit via GuestService")
	timeout := flag.Duration("timeout", 2*time.Hour, "snapshot and publication deadline")
	flag.Parse()
	if *source == "" || *auditPath == "" || *registry == "" || *key == "" || *out == "" || !*stopped || !*clean || !strings.HasPrefix(*id, "base-") || len(*id) > 128 || strings.Trim(*id, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || *timeout <= 0 || *timeout > 24*time.Hour {
		fmt.Fprintln(os.Stderr, "required: --source --audit --registry --registry-secret-file --id base-... --output --stopped --clean-install-reviewed; timeout must be in (0,24h]")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, *timeout)
	defer deadline()
	if err := run(ctx, *source, *auditPath, *registry, *key, *id, *out); err != nil {
		// Registry/OS errors can contain credentials, paths or server response bodies.
		fmt.Fprintln(os.Stderr, "base publication failed; no verified receipt (check private operator diagnostics)")
		os.Exit(1)
	}
}

func readAudit(path string) (audit, string, error) {
	var a audit
	f, err := os.Open(path)
	if err != nil {
		return a, "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return a, "", fmt.Errorf("invalid audit size")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&a); err != nil {
		return a, "", err
	}
	if dec.Decode(new(any)) != io.EOF || a.Version != 1 || a.Status != "pass" || a.Architecture != "arm64" {
		return a, "", fmt.Errorf("audit rejected")
	}
	for _, hash := range []string{a.AgentSHA256, a.PlistSHA256, a.ToolchainSHA256, a.HomeBaselineSHA256, a.AuditScriptSHA256} {
		if !hashValid(hash) {
			return a, "", fmt.Errorf("audit hash rejected")
		}
	}
	if !regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`).MatchString(a.OSVersion) ||
		!regexp.MustCompile(`^[a-zA-Z0-9]+$`).MatchString(a.OSBuild) ||
		!regexp.MustCompile(`^Xcode [0-9.]+; Build version [a-zA-Z0-9]+$`).MatchString(a.ToolchainVersion) {
		return a, "", fmt.Errorf("audit provenance rejected")
	}
	sum := sha256.Sum256(data)
	return a, hex.EncodeToString(sum[:]), nil
}

type deadlineReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r deadlineReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func hashValid(s string) bool { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }

func validateSource(root string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("private real bundle directory required")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 3 {
		return fmt.Errorf("only disk.img, aux.img and config.json allowed")
	}
	for _, name := range []string{"disk.img", "aux.img", "config.json"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("private regular payload required")
		}
	}
	platform, err := images.ValidateMacOSBundle(root)
	if err != nil {
		return err
	}
	if !platform.GuestAgent {
		return fmt.Errorf("system guest agent required")
	}
	return nil
}

func run(ctx context.Context, source, auditPath, registry, key, id, out string) error {
	a, auditHash, err := readAudit(auditPath)
	if err != nil {
		return err
	}
	if err = validateSource(source); err != nil {
		return err
	}
	// Reserve before publishing: never overwrite a prior release receipt.
	output, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		output.Close()
		if !complete {
			os.Remove(out)
		}
	}()
	info, err := os.Lstat(key)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 32 || info.Size() > 4096 {
		return fmt.Errorf("private registry signing key required")
	}
	secret, err := os.ReadFile(key)
	if err != nil {
		return err
	}
	secretText := strings.TrimSpace(string(secret))
	if len(secretText) < 32 {
		return fmt.Errorf("registry signing key is too short")
	}
	workspace, err := os.MkdirTemp("", "hypeman-base-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	bundle, err := snapshot(ctx, source, workspace)
	if err != nil {
		return err
	}
	if err = validateSource(bundle); err != nil {
		return err
	}
	hashes := map[string]string{}
	for _, name := range []string{"disk.img", "aux.img", "config.json"} {
		f, err := os.Open(filepath.Join(bundle, name))
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, deadlineReader{ctx, f})
		f.Close()
		if err != nil {
			return err
		}
		hashes[name] = hex.EncodeToString(h.Sum(nil))
	}
	reference, digest, err := publish(ctx, registry, secretText, id, bundle)
	if err != nil {
		return err
	}
	r := receipt{1, reference, digest, a, auditHash, hashes, time.Now().UTC()}
	if err = json.NewEncoder(output).Encode(r); err != nil {
		return err
	}
	if err = output.Sync(); err != nil {
		return err
	}
	if err = output.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}
