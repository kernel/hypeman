package oidcfederation

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type resolverFixture struct {
	ips   []netip.Addr
	calls int
}

func (r *resolverFixture) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	return r.ips, nil
}

func TestPublicDialUsesTheValidatedAddresses(t *testing.T) {
	resolver := &resolverFixture{ips: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}
	var target string
	dial := publicDial(resolver, func(_ context.Context, network, address string) (net.Conn, error) {
		target = address
		left, right := net.Pipe()
		right.Close()
		return left, nil
	})
	conn, err := dial(context.Background(), "tcp", "identity.example.test:443")
	require.NoError(t, err)
	conn.Close()
	require.Equal(t, "8.8.8.8:443", target)
	require.Equal(t, 1, resolver.calls, "the dialer must not resolve the hostname again")
}

func TestPublicDialRejectsMixedAndPrivateAnswers(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.100.100.200", "0.0.0.1", "192.0.2.1", "224.0.0.1", "::1", "fe80::1", "fc00::1", "::ffff:127.0.0.1", "2001:db8::1"} {
		t.Run(address, func(t *testing.T) {
			resolver := &resolverFixture{ips: []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(address)}}
			called := false
			dial := publicDial(resolver, func(context.Context, string, string) (net.Conn, error) { called = true; return nil, nil })
			_, err := dial(context.Background(), "tcp", "identity.example.test:443")
			var problem *KeyProblem
			require.ErrorAs(t, err, &problem, "a blocked address is a configuration problem, not an outage")
			require.ErrorIs(t, err, ErrUnavailable, "the token endpoint still sees only unavailability")
			require.False(t, called)
		})
	}
}

func TestKeyHTTPClientCannotUseAnAmbientProxyOrRedirect(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9999")
	client, err := keyHTTPClient("")
	require.NoError(t, err)
	defer client.CloseIdleConnections()
	transport := client.Transport.(*http.Transport)
	require.Nil(t, transport.Proxy)
	require.True(t, transport.DisableCompression)
	require.Equal(t, http.ErrUseLastResponse, client.CheckRedirect(&http.Request{}, nil))
}

func TestKeyDocumentStatusTypeEncodingAndSizeBounds(t *testing.T) {
	reasons := map[string]string{"redirect": "HTTP 302", "missing": "HTTP 404", "html": "did not return JSON", "compressed": "compressed", "large": "larger than"}
	for _, mode := range []string{"ok", "redirect", "missing", "outage", "html", "compressed", "large"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Empty(t, r.Header.Get("Authorization"))
				require.Empty(t, r.Header.Get("Cookie"))
				w.Header().Set("Content-Type", "application/json")
				switch mode {
				case "redirect":
					w.Header().Set("Location", "/other")
					w.WriteHeader(http.StatusFound)
				case "missing":
					w.WriteHeader(http.StatusNotFound)
				case "outage":
					w.WriteHeader(http.StatusBadGateway)
				case "html":
					w.Header().Set("Content-Type", "text/html")
					w.Write([]byte("not-json"))
				case "compressed":
					w.Header().Set("Content-Encoding", "gzip")
					w.Write([]byte("not-gzip"))
				case "large":
					w.Write([]byte(strings.Repeat("x", maxKeyDocumentBytes+1)))
				default:
					w.Write([]byte(`{"keys":[]}`))
				}
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			client.Transport.(*http.Transport).DisableCompression = true // as keyHTTPClient does
			_, err := fetchKeyDocument(context.Background(), client, server.URL+"?public-policy=example")
			var problem *KeyProblem
			switch mode {
			case "ok":
				require.NoError(t, err)
			case "outage":
				require.ErrorIs(t, err, ErrUnavailable)
				require.False(t, errors.As(err, &problem), "a server error may pass, so it is not a configuration problem")
			default:
				require.ErrorIs(t, err, ErrUnavailable)
				require.ErrorAs(t, err, &problem)
				require.Contains(t, problem.Reason, reasons[mode])
			}
		})
	}
}

func TestConfiguredCAReplacesSystemRoots(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	ca := server.Certificate()
	client, err := keyHTTPClient(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})))
	require.NoError(t, err)
	only := x509.NewCertPool()
	only.AddCert(ca)
	require.True(t, client.Transport.(*http.Transport).TLSClientConfig.RootCAs.Equal(only))

	client, err = keyHTTPClient("")
	require.NoError(t, err)
	require.Nil(t, client.Transport.(*http.Transport).TLSClientConfig.RootCAs, "without a configured CA the system roots apply")
}

func TestOnlyPublicCACertificatesCanBeConfigured(t *testing.T) {
	for _, data := range []string{" ", "not-a-certificate", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("synthetic")}))} {
		require.Error(t, validateCA(data))
	}
}
