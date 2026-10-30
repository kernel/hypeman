package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/images"
)

func goodAudit() audit {
	h := strings.Repeat("a", 64)
	return audit{Version: 1, Status: "pass", OSVersion: "15.5", OSBuild: "24F74", Architecture: "arm64", AgentSHA256: h, PlistSHA256: h, ToolchainSHA256: h, ToolchainVersion: "Xcode 16.4; Build version 16F6", HomeBaselineSHA256: h, AuditScriptSHA256: h}
}
func TestReadAudit(t *testing.T) {
	a := goodAudit()
	data, _ := json.Marshal(a)
	tests := map[string][]byte{"valid": data, "trailing": append(append([]byte{}, data...), []byte(" {}")...), "unknown": []byte(strings.Replace(string(data), `"status":"pass"`, `"status":"pass","credential":"secret"`, 1)), "oversized": []byte(strings.Repeat(" ", 65537))}
	bad := a
	bad.Status = "fail"
	tests["failed"], _ = json.Marshal(bad)
	bad = a
	bad.OSVersion = "secret-or-command-output"
	tests["unsafe-provenance"], _ = json.Marshal(bad)
	bad = a
	bad.AgentSHA256 = ""
	tests["missing-pin"], _ = json.Marshal(bad)
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.json")
			os.WriteFile(path, data, 0600)
			_, sum, err := readAudit(path)
			if name == "valid" {
				if err != nil || !hashValid(sum) {
					t.Fatalf("valid audit rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid audit accepted")
			}
		})
	}
}
func TestDeadlineReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := io.ReadAll(deadlineReader{ctx, strings.NewReader("data")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestFailedPublicationEvidence(t *testing.T) {
	root := bundleFixture(t)
	dir := t.TempDir()
	a, _ := json.Marshal(goodAudit())
	auditPath := filepath.Join(dir, "audit.json")
	if err := os.WriteFile(auditPath, a, 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "receipt.json")
	err := run(context.Background(), root, auditPath, "https://registry.example", filepath.Join(dir, "missing-key"), "base-test", out)
	if err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("failed publication left receipt")
	}
	if err := os.WriteFile(out, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), root, auditPath, "https://registry.example", "missing-key", "base-test", out); err == nil {
		t.Fatal("existing receipt accepted")
	}
	data, _ := os.ReadFile(out)
	if string(data) != "existing" {
		t.Fatal("existing receipt changed")
	}
}

func bundleFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.Chmod(root, 0700)
	platform := images.MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30, GuestAgent: true}
	data, _ := json.Marshal(platform)
	for name, data := range map[string][]byte{"disk.img": []byte("disk"), "aux.img": []byte("aux"), "config.json": data} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func TestValidateSource(t *testing.T) {
	for _, name := range []string{"valid", "extra", "symlink", "public", "unmanaged", "unknown-metadata", "hardlink"} {
		t.Run(name, func(t *testing.T) {
			root := bundleFixture(t)
			switch name {
			case "extra":
				os.WriteFile(filepath.Join(root, "token"), []byte("secret"), 0600)
			case "symlink":
				os.Remove(filepath.Join(root, "aux.img"))
				os.Symlink("disk.img", filepath.Join(root, "aux.img"))
			case "hardlink":
				os.Remove(filepath.Join(root, "aux.img"))
				os.Link(filepath.Join(root, "disk.img"), filepath.Join(root, "aux.img"))
			case "public":
				os.Chmod(filepath.Join(root, "disk.img"), 0644)
			case "unmanaged", "unknown-metadata":
				data, _ := os.ReadFile(filepath.Join(root, "config.json"))
				if name == "unmanaged" {
					data = []byte(strings.Replace(string(data), `"guest_agent":true`, `"guest_agent":false`, 1))
				} else {
					data = []byte(strings.Replace(string(data), `"guest_agent":true`, `"guest_agent":true,"token":"secret"`, 1))
				}
				os.WriteFile(filepath.Join(root, "config.json"), data, 0600)
			}
			err := validateSource(root)
			if (err == nil) != (name == "valid") {
				t.Fatalf("validation result: %v", err)
			}
		})
	}
}
