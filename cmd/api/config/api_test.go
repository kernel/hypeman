package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadAPIListenPorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8080\napi:\n  listen_ports: [443, 4973]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.API.ListenPorts, []int{443, 4973}) {
		t.Fatalf("unexpected API listen ports: %v", cfg.API.ListenPorts)
	}
}

func TestValidateAPIListenPorts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		ports   []int
		tls     bool
		backend string
		want    string
	}{
		{"defaults", nil, true, "4973", ""},
		{"empty", []int{}, true, "4973", ""},
		{"current", nil, true, "8080", ""},
		{"explicit current", []int{443}, true, "8080", ""},
		{"dual TLS", []int{443, 4973}, true, "8080", ""},
		{"custom TLS", []int{4973}, true, "8080", ""},
		{"default HTTP", nil, false, "4973", ""},
		{"empty HTTP", []int{}, false, "4973", ""},
		{"explicit HTTP", []int{80}, false, "8080", ""},
		{"custom HTTP", []int{8081, 8082}, false, "8080", ""},
		{"TLS on 80", []int{80}, true, "8080", "cannot include HTTP port 80"},
		{"zero", []int{0}, true, "8080", "between 1 and 65535"},
		{"negative", []int{-1}, true, "8080", "between 1 and 65535"},
		{"too large", []int{65536}, true, "8080", "between 1 and 65535"},
		{"duplicate", []int{443, 443}, true, "8080", "duplicate port 443"},
		{"default backend collision", []int{443, 4973}, true, "4973", "conflicts with backend port"},
		{"HTTP backend collision", []int{8080}, false, "8080", "conflicts with backend port"},
		{"metrics collision", []int{9464}, true, "8080", "conflicts with metrics.port"},
		{"admin collision", []int{2019}, true, "8080", "conflicts with caddy.admin_port"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Port = tt.backend
			cfg.API = APIConfig{Hostname: "api.example.com", TLS: tt.tls, ListenPorts: tt.ports}
			cfg.Caddy.AdminPort = 2019
			err := cfg.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestAPIListenPortsSeparateBindings(t *testing.T) {
	cfg := defaultConfig()
	cfg.Port = "8080"
	cfg.ListenAddress = "127.0.0.1"
	cfg.Caddy.ListenAddress = "192.0.2.1"
	cfg.Caddy.AdminPort = 2019
	cfg.API = APIConfig{Hostname: "api.example.com", TLS: true, ListenPorts: []int{8080, 9464, 2019}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBindAddressesOverlap(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"", "127.0.0.1", true},
		{"0.0.0.0", "127.0.0.1", true},
		{"::", "::1", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"127.0.0.1", "::ffff:127.0.0.1", true},
		{"localhost", "127.0.0.1", true},
		{"192.0.2.1", "127.0.0.1", false},
		{"2001:db8::1", "::1", false},
	} {
		t.Run(tt.a+"/"+tt.b, func(t *testing.T) {
			if got := bindAddressesOverlap(tt.a, tt.b); got != tt.want {
				t.Fatalf("overlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
