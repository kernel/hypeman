package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadAPIHTTPSPorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte("port: 8080\napi:\n  hostname: api.example.com\n  https_ports: [443, 4973]\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.API.HTTPSPorts, []int{443, 4973}) {
		t.Fatalf("unexpected API HTTPS ports: %v", cfg.API.HTTPSPorts)
	}
}

func TestValidateAPIHTTPSPorts(t *testing.T) {
	for _, tt := range []struct {
		name  string
		ports []int
		valid bool
	}{
		{"default", nil, true},
		{"empty", []int{}, true},
		{"dual", []int{443, 4973}, true},
		{"custom only", []int{4973}, true},
		{"http port", []int{80}, false},
		{"http port with https", []int{443, 80}, false},
		{"zero", []int{0}, false},
		{"negative", []int{-1}, false},
		{"too large", []int{65536}, false},
		{"duplicate", []int{443, 443}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Port = "8080"
			cfg.API.Hostname = "api.example.com"
			cfg.API.HTTPSPorts = tt.ports
			err := cfg.Validate()
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "api.https_ports") {
				t.Fatalf("expected api.https_ports validation error, got %v", err)
			}
		})
	}
}
