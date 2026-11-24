package oidcfederation

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const maxKeyDocumentBytes = 256 << 10

// KeyProblem says why an issuer's keys cannot be used, for the operator who
// configured it. Retrying does not help. To the token endpoint it reads as
// ErrUnavailable, so a caller learns nothing about the issuer.
type KeyProblem struct{ Reason string }

func (p *KeyProblem) Error() string { return "issuer keys cannot be used: " + p.Reason }

func (p *KeyProblem) Is(target error) bool { return target == ErrUnavailable }

type publicResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// nonPublicPrefixes are the ranges that are not globally reachable: private,
// loopback, link-local, multicast, and the other entries of the IANA
// special-purpose address registries.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),      // private
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (CGNAT)
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local
	netip.MustParsePrefix("172.16.0.0/12"),   // private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast
	netip.MustParsePrefix("192.168.0.0/16"),  // private
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, including broadcast
	netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4
	netip.MustParsePrefix("3fff::/20"),       // documentation
}

// globalUnicastIPv6 excludes the IPv6 loopback, ULA, link-local, multicast and
// IPv4-mapped ranges.
var globalUnicastIPv6 = netip.MustParsePrefix("2000::/3")

func publicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || (addr.Is6() && !globalUnicastIPv6.Contains(addr)) {
		return false
	}
	for _, nonPublic := range nonPublicPrefixes {
		if nonPublic.Contains(addr) {
			return false
		}
	}
	return true
}

func publicDial(resolver publicResolver, dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrUnavailable
		}
		ips, err := resolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 || len(ips) > 32 {
			return nil, ErrUnavailable
		}
		for _, ip := range ips {
			if !publicAddress(ip) {
				return nil, &KeyProblem{"the address is not on the public internet"}
			}
		}
		for _, ip := range ips {
			conn, err := dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, ErrUnavailable
	}
}

func validateCA(caPEM string) error {
	if caPEM == "" {
		return nil
	}
	if len(caPEM) > 64<<10 {
		return fmt.Errorf("%w: CA certificate is too large", ErrInvalid)
	}
	data := []byte(strings.TrimSpace(caPEM))
	count := 0
	for len(data) > 0 {
		if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----")) {
			return fmt.Errorf("%w: only public CA certificates are accepted", ErrInvalid)
		}
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return fmt.Errorf("%w: only public CA certificates are accepted", ErrInvalid)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !certificate.IsCA || !certificate.BasicConstraintsValid {
			return fmt.Errorf("%w: invalid CA certificate", ErrInvalid)
		}
		count++
		if count > 16 {
			return fmt.Errorf("%w: too many CA certificates", ErrInvalid)
		}
		data = []byte(strings.TrimSpace(string(rest)))
	}
	if count == 0 {
		return fmt.Errorf("%w: CA certificate is required", ErrInvalid)
	}
	return nil
}

func keyHTTPClient(caPEM string) (*http.Client, error) {
	if err := validateCA(caPEM); err != nil {
		return nil, err
	}
	// A configured CA replaces the system roots, so only it can vouch for the key endpoint.
	var roots *x509.CertPool
	if caPEM != "" {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, fmt.Errorf("%w: invalid CA certificate", ErrInvalid)
		}
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            publicDial(net.DefaultResolver, dialer.DialContext),
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout:    3 * time.Second,
		ResponseHeaderTimeout:  5 * time.Second,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 16 << 10,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func fetchKeyDocument(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	if _, err := httpsURL(endpoint); err != nil {
		return nil, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Accept", "application/json, application/jwk-set+json")
	response, err := client.Do(req)
	if err != nil {
		var problem *KeyProblem
		if errors.As(err, &problem) {
			return nil, problem
		}
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode >= 500 {
		return nil, ErrUnavailable
	}
	if response.StatusCode != http.StatusOK {
		return nil, &KeyProblem{fmt.Sprintf("the key endpoint returned HTTP %d", response.StatusCode)}
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (media != "application/json" && !strings.HasSuffix(media, "+json")) {
		return nil, &KeyProblem{"the key endpoint did not return JSON"}
	}
	if response.Header.Get("Content-Encoding") != "" {
		return nil, &KeyProblem{"the key endpoint returned a compressed response"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxKeyDocumentBytes+1))
	if err != nil {
		return nil, ErrUnavailable
	}
	if len(body) > maxKeyDocumentBytes {
		return nil, &KeyProblem{"the key document is larger than 256 KB"}
	}
	return body, nil
}

func fetchIssuerKeys(ctx context.Context, issuer IssuerSpec) ([]byte, error) {
	if issuer.JWKS.Type == "inline" {
		return json.Marshal(map[string]any{"keys": issuer.JWKS.Keys})
	}
	client, err := keyHTTPClient(issuer.JWKS.CACertPEM)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	endpoint := issuer.JWKS.URL
	if issuer.JWKS.Type == "discovery" {
		base := issuer.IssuerURL
		if issuer.JWKS.DiscoveryBaseURL != "" {
			base = issuer.JWKS.DiscoveryBaseURL
		}
		raw, err := fetchKeyDocument(ctx, client, strings.TrimRight(base, "/")+"/.well-known/openid-configuration")
		if err != nil {
			return nil, err
		}
		metadata, err := decodeDiscovery(raw)
		if err != nil {
			return nil, &KeyProblem{"the discovery document is not valid"}
		}
		if metadata.Issuer != issuer.IssuerURL {
			return nil, &KeyProblem{"the discovery document names a different issuer"}
		}
		endpoint = metadata.JWKSURI
	}
	if endpoint == "" {
		return nil, &KeyProblem{"no key URL is configured or published"}
	}
	return fetchKeyDocument(ctx, client, endpoint)
}
