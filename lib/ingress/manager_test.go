package ingress

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInstanceResolver implements InstanceResolver for testing
type mockInstanceResolver struct {
	instances map[string]string // instance name/ID -> IP
}

func newMockResolver() *mockInstanceResolver {
	return &mockInstanceResolver{
		instances: make(map[string]string),
	}
}

func (m *mockInstanceResolver) AddInstance(nameOrID, ip string) {
	m.instances[nameOrID] = ip
}

func (m *mockInstanceResolver) ResolveInstanceIP(ctx context.Context, nameOrID string) (string, error) {
	ip, ok := m.instances[nameOrID]
	if !ok {
		return "", ErrInstanceNotFound
	}
	return ip, nil
}

func (m *mockInstanceResolver) InstanceExists(ctx context.Context, nameOrID string) (bool, error) {
	_, ok := m.instances[nameOrID]
	return ok, nil
}

func setupTestManager(t *testing.T) (Manager, *mockInstanceResolver, *paths.Paths, func()) {
	t.Helper()

	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-manager-test-*")
	require.NoError(t, err)

	p := paths.New(tmpDir)

	// Create required directories
	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))
	require.NoError(t, os.MkdirAll(p.IngressesDir(), 0755))

	resolver := newMockResolver()
	resolver.AddInstance("my-api", "10.100.0.10")
	resolver.AddInstance("web-app", "10.100.0.20")

	config := Config{
		ListenAddress:  "0.0.0.0",
		AdminAddress:   "127.0.0.1",
		AdminPort:      12019, // Use different port for testing
		StopOnShutdown: true,
		// Empty ACME config - TLS not configured for basic tests
	}

	// Pass nil for otelLogger - no log forwarding in tests
	manager := NewManager(p, config, resolver, nil)

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return manager, resolver, p, cleanup
}

func TestCreateIngress_Success(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateIngressRequest{
		Name: "test-ingress",
		Rules: []IngressRule{
			{
				Match:  IngressMatch{Hostname: "api.example.com"},
				Target: IngressTarget{Instance: "my-api", Port: 8080},
			},
		},
	}

	ing, err := manager.Create(ctx, req)
	require.NoError(t, err)
	assert.NotEmpty(t, ing.ID)
	assert.Equal(t, "test-ingress", ing.Name)
	assert.Len(t, ing.Rules, 1)
	assert.Equal(t, "api.example.com", ing.Rules[0].Match.Hostname)
	assert.WithinDuration(t, time.Now(), ing.CreatedAt, time.Second)
}

func TestCreateIngress_MultipleRules(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateIngressRequest{
		Name: "multi-rule-ingress",
		Rules: []IngressRule{
			{
				Match:  IngressMatch{Hostname: "api.example.com"},
				Target: IngressTarget{Instance: "my-api", Port: 8080},
			},
			{
				Match:  IngressMatch{Hostname: "web.example.com"},
				Target: IngressTarget{Instance: "web-app", Port: 3000},
			},
		},
	}

	ing, err := manager.Create(ctx, req)
	require.NoError(t, err)
	assert.Len(t, ing.Rules, 2)
}

func TestCreateIngress_CustomPort(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateIngressRequest{
		Name: "custom-port-ingress",
		Rules: []IngressRule{
			{
				Match:  IngressMatch{Hostname: "api.example.com", Port: 8080},
				Target: IngressTarget{Instance: "my-api", Port: 3000},
			},
		},
	}

	ing, err := manager.Create(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, 8080, ing.Rules[0].Match.Port)
	assert.Equal(t, 8080, ing.Rules[0].Match.GetPort())
}

func TestCreateIngress_DefaultPort(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateIngressRequest{
		Name: "default-port-ingress",
		Rules: []IngressRule{
			{
				Match:  IngressMatch{Hostname: "api.example.com"}, // Port not specified
				Target: IngressTarget{Instance: "my-api", Port: 3000},
			},
		},
	}

	ing, err := manager.Create(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, 0, ing.Rules[0].Match.Port)       // Stored as 0
	assert.Equal(t, 80, ing.Rules[0].Match.GetPort()) // But GetPort returns 80
}

func TestCreateIngress_InvalidName(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	testCases := []struct {
		name string
	}{
		{"Invalid_Name"},
		{"-starts-with-dash"},
		{"ends-with-dash-"},
		{"has spaces"},
		{"UPPERCASE"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := CreateIngressRequest{
				Name: tc.name,
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
				},
			}

			_, err := manager.Create(ctx, req)
			assert.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidRequest)
		})
	}
}

func TestCreateIngress_EmptyName(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateIngressRequest{
		Name: "",
		Rules: []IngressRule{
			{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
		},
	}

	_, err := manager.Create(ctx, req)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
}

func TestCreateIngress_NoRules(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateIngressRequest{
		Name:  "no-rules-ingress",
		Rules: []IngressRule{},
	}

	_, err := manager.Create(ctx, req)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
}

func TestCreateIngress_InstanceNotFound(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateIngressRequest{
		Name: "missing-instance",
		Rules: []IngressRule{
			{
				Match:  IngressMatch{Hostname: "test.example.com"},
				Target: IngressTarget{Instance: "nonexistent-instance", Port: 8080},
			},
		},
	}

	_, err := manager.Create(ctx, req)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestCreateIngress_DuplicateName(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateIngressRequest{
		Name: "unique-ingress",
		Rules: []IngressRule{
			{Match: IngressMatch{Hostname: "first.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
		},
	}

	// Create first ingress
	_, err := manager.Create(ctx, req)
	require.NoError(t, err)

	// Try to create another with same name but different hostname
	req.Rules[0].Match.Hostname = "second.example.com"
	_, err = manager.Create(ctx, req)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrAlreadyExists)
}

func TestCreateIngress_DuplicateHostname(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create first ingress with hostname
	req1 := CreateIngressRequest{
		Name: "first-ingress",
		Rules: []IngressRule{
			{Match: IngressMatch{Hostname: "shared.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
		},
	}
	_, err := manager.Create(ctx, req1)
	require.NoError(t, err)

	// Try to create another ingress with same hostname
	req2 := CreateIngressRequest{
		Name: "second-ingress",
		Rules: []IngressRule{
			{Match: IngressMatch{Hostname: "shared.example.com"}, Target: IngressTarget{Instance: "web-app", Port: 3000}},
		},
	}
	_, err = manager.Create(ctx, req2)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrHostnameInUse)
}

func TestGetIngress_ByID(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create ingress
	req := CreateIngressRequest{
		Name: "get-test",
		Rules: []IngressRule{
			{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
		},
	}
	created, err := manager.Create(ctx, req)
	require.NoError(t, err)

	// Get by ID
	found, err := manager.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, found.ID)
	assert.Equal(t, created.Name, found.Name)
}

func TestGetIngress_ByName(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create ingress
	req := CreateIngressRequest{
		Name: "named-ingress",
		Rules: []IngressRule{
			{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
		},
	}
	created, err := manager.Create(ctx, req)
	require.NoError(t, err)

	// Get by name
	found, err := manager.Get(ctx, "named-ingress")
	require.NoError(t, err)
	assert.Equal(t, created.ID, found.ID)
}

func TestGetIngress_NotFound(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	_, err := manager.Get(ctx, "nonexistent")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListIngresses_Empty(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	ingresses, err := manager.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, ingresses)
}

func TestListIngresses_Multiple(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create multiple ingresses
	for i := 0; i < 3; i++ {
		req := CreateIngressRequest{
			Name: "ingress-" + string(rune('a'+i)),
			Rules: []IngressRule{
				{Match: IngressMatch{Hostname: "host" + string(rune('0'+i)) + ".example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
			},
		}
		_, err := manager.Create(ctx, req)
		require.NoError(t, err)
	}

	ingresses, err := manager.List(ctx)
	require.NoError(t, err)
	assert.Len(t, ingresses, 3)
}

func TestDeleteIngress_ByID(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create ingress
	req := CreateIngressRequest{
		Name: "delete-test",
		Rules: []IngressRule{
			{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
		},
	}
	created, err := manager.Create(ctx, req)
	require.NoError(t, err)

	// Delete by ID
	err = manager.Delete(ctx, created.ID)
	require.NoError(t, err)

	// Verify deleted
	_, err = manager.Get(ctx, created.ID)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteIngress_ByName(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create ingress
	req := CreateIngressRequest{
		Name: "delete-by-name",
		Rules: []IngressRule{
			{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
		},
	}
	_, err := manager.Create(ctx, req)
	require.NoError(t, err)

	// Delete by name
	err = manager.Delete(ctx, "delete-by-name")
	require.NoError(t, err)

	// Verify deleted
	_, err = manager.Get(ctx, "delete-by-name")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteIngress_NotFound(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	err := manager.Delete(ctx, "nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestValidateName(t *testing.T) {
	validNames := []string{
		"a",
		"ab",
		"my-ingress",
		"ingress-1",
		"a1b2c3",
		"test123",
	}

	invalidNames := []string{
		"",
		"-starts-with-dash",
		"ends-with-dash-",
		"has spaces",
		"UPPERCASE",
		"has_underscore",
		"has.period",
	}

	for _, name := range validNames {
		t.Run("valid:"+name, func(t *testing.T) {
			assert.True(t, isValidName(name), "expected %q to be valid", name)
		})
	}

	for _, name := range invalidNames {
		t.Run("invalid:"+name, func(t *testing.T) {
			assert.False(t, isValidName(name), "expected %q to be invalid", name)
		})
	}
}

func TestCreateIngressRequest_Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     CreateIngressRequest
		wantErr bool
	}{
		{
			name: "valid request",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
				},
			},
			wantErr: false,
		},
		{
			name: "empty name",
			req: CreateIngressRequest{
				Name: "",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
				},
			},
			wantErr: true,
		},
		{
			name: "empty rules",
			req: CreateIngressRequest{
				Name:  "valid",
				Rules: []IngressRule{},
			},
			wantErr: true,
		},
		{
			name: "empty hostname",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: ""}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
				},
			},
			wantErr: true,
		},
		{
			name: "empty instance",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "", Port: 8080}},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid port zero",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 0}},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid port negative",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: -1}},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid port too high",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 70000}},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid match port too high",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com", Port: 70000}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid match port negative",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com", Port: -1}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
				},
			},
			wantErr: true,
		},
		{
			name: "valid with custom match port",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com", Port: 8080}, Target: IngressTarget{Instance: "my-api", Port: 3000}},
				},
			},
			wantErr: false,
		},
		{
			name: "wildcard hostname not supported",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "*.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}},
				},
			},
			wantErr: true,
		},
		{
			name: "redirect_http without tls",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "my-api", Port: 8080}, RedirectHTTP: true, TLS: false},
				},
			},
			wantErr: true,
		},
		{
			name: "valid tls with redirect",
			req: CreateIngressRequest{
				Name: "valid",
				Rules: []IngressRule{
					{Match: IngressMatch{Hostname: "test.example.com", Port: 443}, Target: IngressTarget{Instance: "my-api", Port: 8080}, TLS: true, RedirectHTTP: true},
				},
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCreateIngress_TLSWithoutACME(t *testing.T) {
	// Setup manager without ACME configured
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Try to create TLS ingress without ACME config
	req := CreateIngressRequest{
		Name: "tls-ingress",
		Rules: []IngressRule{
			{
				Match:  IngressMatch{Hostname: "secure.example.com", Port: 443},
				Target: IngressTarget{Instance: "my-api", Port: 8080},
				TLS:    true,
			},
		},
	}

	_, err := manager.Create(ctx, req)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "ACME is not configured")
}

func TestGetIngress_Resolution(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-resolution-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.IngressesDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))

	ctx := context.Background()

	// Directly save some test ingresses to storage
	ingress1 := &storedIngress{
		ID:        "abc123def456",
		Name:      "api-ingress",
		Rules:     []IngressRule{{Match: IngressMatch{Hostname: "api.example.com", Port: 80}, Target: IngressTarget{Instance: "api", Port: 8080}}},
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	ingress2 := &storedIngress{
		ID:        "abc789xyz123",
		Name:      "web-ingress",
		Rules:     []IngressRule{{Match: IngressMatch{Hostname: "web.example.com", Port: 80}, Target: IngressTarget{Instance: "web", Port: 8080}}},
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	ingress3 := &storedIngress{
		ID:        "xyz999aaa111",
		Name:      "admin-ingress",
		Rules:     []IngressRule{{Match: IngressMatch{Hostname: "admin.example.com", Port: 80}, Target: IngressTarget{Instance: "admin", Port: 8080}}},
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	require.NoError(t, saveIngress(p, ingress1))
	require.NoError(t, saveIngress(p, ingress2))
	require.NoError(t, saveIngress(p, ingress3))

	resolver := newMockResolver()
	config := Config{
		ListenAddress:  "0.0.0.0",
		AdminAddress:   "127.0.0.1",
		AdminPort:      12019,
		StopOnShutdown: true,
	}
	manager := NewManager(p, config, resolver, nil)

	t.Run("exact ID match", func(t *testing.T) {
		ing, err := manager.Get(ctx, "abc123def456")
		require.NoError(t, err)
		assert.Equal(t, "abc123def456", ing.ID)
		assert.Equal(t, "api-ingress", ing.Name)
	})

	t.Run("exact name match", func(t *testing.T) {
		ing, err := manager.Get(ctx, "web-ingress")
		require.NoError(t, err)
		assert.Equal(t, "abc789xyz123", ing.ID)
		assert.Equal(t, "web-ingress", ing.Name)
	})

	t.Run("unique ID prefix match", func(t *testing.T) {
		ing, err := manager.Get(ctx, "xyz")
		require.NoError(t, err)
		assert.Equal(t, "xyz999aaa111", ing.ID)
		assert.Equal(t, "admin-ingress", ing.Name)
	})

	t.Run("longer unique ID prefix", func(t *testing.T) {
		ing, err := manager.Get(ctx, "abc123")
		require.NoError(t, err)
		assert.Equal(t, "abc123def456", ing.ID)
	})

	t.Run("ambiguous ID prefix", func(t *testing.T) {
		// "abc" matches both abc123def456 and abc789xyz123
		_, err := manager.Get(ctx, "abc")
		assert.ErrorIs(t, err, ErrAmbiguousName)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := manager.Get(ctx, "nonexistent")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("ID takes precedence over name prefix", func(t *testing.T) {
		// If we have an exact ID match, it should be returned even if
		// there's another ingress with a name starting with the same prefix
		ing, err := manager.Get(ctx, "abc123def456")
		require.NoError(t, err)
		assert.Equal(t, "abc123def456", ing.ID)
	})
}

func TestDeleteIngress_Resolution(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-delete-resolution-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.IngressesDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))

	ctx := context.Background()

	resolver := newMockResolver()
	config := Config{
		ListenAddress:  "0.0.0.0",
		AdminAddress:   "127.0.0.1",
		AdminPort:      12019,
		StopOnShutdown: true,
	}

	t.Run("delete by name", func(t *testing.T) {
		// Save test ingress
		ingress := &storedIngress{
			ID:        "del123abc",
			Name:      "delete-by-name",
			Rules:     []IngressRule{{Match: IngressMatch{Hostname: "test.example.com", Port: 80}, Target: IngressTarget{Instance: "api", Port: 8080}}},
			CreatedAt: time.Now().Format(time.RFC3339),
		}
		require.NoError(t, saveIngress(p, ingress))

		manager := NewManager(p, config, resolver, nil)
		err := manager.Delete(ctx, "delete-by-name")
		require.NoError(t, err)

		// Verify it's gone
		_, err = manager.Get(ctx, "del123abc")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("delete by ID prefix", func(t *testing.T) {
		// Save test ingress
		ingress := &storedIngress{
			ID:        "unique999prefix",
			Name:      "prefix-delete-test",
			Rules:     []IngressRule{{Match: IngressMatch{Hostname: "test2.example.com", Port: 80}, Target: IngressTarget{Instance: "api", Port: 8080}}},
			CreatedAt: time.Now().Format(time.RFC3339),
		}
		require.NoError(t, saveIngress(p, ingress))

		manager := NewManager(p, config, resolver, nil)
		err := manager.Delete(ctx, "unique999")
		require.NoError(t, err)

		// Verify it's gone
		_, err = manager.Get(ctx, "unique999prefix")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("delete ambiguous prefix fails", func(t *testing.T) {
		// Save two ingresses with similar IDs
		ingress1 := &storedIngress{
			ID:        "ambig111aaa",
			Name:      "ambig-test-1",
			Rules:     []IngressRule{{Match: IngressMatch{Hostname: "ambig1.example.com", Port: 80}, Target: IngressTarget{Instance: "api", Port: 8080}}},
			CreatedAt: time.Now().Format(time.RFC3339),
		}
		ingress2 := &storedIngress{
			ID:        "ambig111bbb",
			Name:      "ambig-test-2",
			Rules:     []IngressRule{{Match: IngressMatch{Hostname: "ambig2.example.com", Port: 80}, Target: IngressTarget{Instance: "api", Port: 8080}}},
			CreatedAt: time.Now().Format(time.RFC3339),
		}
		require.NoError(t, saveIngress(p, ingress1))
		require.NoError(t, saveIngress(p, ingress2))

		manager := NewManager(p, config, resolver, nil)
		err := manager.Delete(ctx, "ambig111")
		assert.ErrorIs(t, err, ErrAmbiguousName)

		// Both should still exist
		_, err = manager.Get(ctx, "ambig111aaa")
		require.NoError(t, err)
		_, err = manager.Get(ctx, "ambig111bbb")
		require.NoError(t, err)
	})
}
