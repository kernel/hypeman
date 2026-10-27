package ingress

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateConfig_APIListenPorts(t *testing.T) {
	for _, tt := range []struct {
		name     string
		ports    []int
		tls      bool
		redirect bool
		listens  []int
		location string
	}{
		{"empty", []int{}, true, true, []int{80, 443}, "https://{http.request.host}{http.request.uri}"},
		{"default", nil, true, true, []int{80, 443}, "https://{http.request.host}{http.request.uri}"},
		{"dual", []int{443, 4973}, true, true, []int{80, 443, 4973}, "https://{http.request.host}{http.request.uri}"},
		{"prefer 443", []int{4973, 443}, true, true, []int{80, 443, 4973}, "https://{http.request.host}{http.request.uri}"},
		{"custom only", []int{4973}, true, true, []int{80, 4973}, "https://{http.request.host}:4973{http.request.uri}"},
		{"no redirect", []int{443, 4973}, true, false, []int{443, 4973}, ""},
		{"http", nil, false, true, []int{80}, ""},
		{"empty http", []int{}, false, true, []int{80}, ""},
		{"custom http", []int{8081}, false, true, []int{8081}, ""},
		{"multiple http", []int{8081, 8082}, false, true, []int{8081, 8082}, ""},
		{"http on 443", []int{443}, false, true, []int{443}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			generator, _, cleanup := setupTestGenerator(t)
			defer cleanup()
			generator.apiIngress = APIIngressConfig{
				Hostname: "api.example.com", Port: 8080, TLS: tt.tls,
				ListenPorts: tt.ports, RedirectHTTP: tt.redirect,
			}
			generator.acme = ACMEConfig{
				Email: "admin@example.com", DNSProvider: DNSProviderCloudflare,
				CloudflareAPIToken: "test-token",
			}

			config, err := generator.buildConfig(context.Background(), nil)
			require.NoError(t, err)
			apps := config["apps"].(map[string]interface{})
			servers := apps["http"].(map[string]interface{})["servers"].(map[string]interface{})
			require.Len(t, servers, len(tt.listens))
			for _, port := range tt.listens {
				server := servers[fmt.Sprintf("ingress-%d", port)].(map[string]interface{})
				assert.Equal(t, []string{fmt.Sprintf("0.0.0.0:%d", port)}, server["listen"])
				routes := server["routes"].([]interface{})
				require.Len(t, routes, 2)
				route := routes[0].(map[string]interface{})
				match := route["match"].([]interface{})[0].(map[string]interface{})
				assert.Equal(t, []string{"api.example.com"}, match["host"])
				handler := route["handle"].([]interface{})[0].(map[string]interface{})
				https := server["automatic_https"].(map[string]interface{})
				if tt.tls && port == 80 {
					assert.Equal(t, "static_response", handler["handler"])
					assert.Equal(t, []string{tt.location}, handler["headers"].(map[string]interface{})["Location"])
					assert.Equal(t, true, https["disable"])
				} else {
					assert.Equal(t, "reverse_proxy", handler["handler"])
					assert.Equal(t, []map[string]interface{}{{"dial": "127.0.0.1:8080"}}, handler["upstreams"])
					if tt.tls {
						assert.Equal(t, true, https["disable_redirects"])
						assert.NotContains(t, https, "disable")
					} else {
						assert.Equal(t, true, https["disable"])
					}
				}
			}
			if tt.tls {
				policies := apps["tls"].(map[string]interface{})["automation"].(map[string]interface{})["policies"].([]interface{})
				require.Len(t, policies, 1)
				assert.Equal(t, []string{"api.example.com"}, policies[0].(map[string]interface{})["subjects"])
			} else {
				assert.NotContains(t, apps, "tls")
			}
		})
	}
}
