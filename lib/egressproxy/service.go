package egressproxy

import (
	"bufio"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type sourcePolicy struct {
	MockToRealEnvVar map[string]string
}

// Service is a host-side per-process HTTP/HTTPS MITM egress proxy.
type Service struct {
	mu sync.RWMutex

	dataDir    string
	gatewayIP  string
	listenPort int

	server   *http.Server
	listener net.Listener

	transport *http.Transport

	caCert *x509.Certificate
	caKey  *rsa.PrivateKey
	caPEM  string

	certCache map[string]*tls.Certificate

	policiesBySourceIP map[string]sourcePolicy
	sourceIPByInstance map[string]string
}

func NewService(dataDir string, listenPort int) (*Service, error) {
	if listenPort <= 0 {
		listenPort = DefaultListenPort
	}

	caCert, caKey, caPEM, err := loadOrCreateCA(dataDir)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	return &Service{
		dataDir:            dataDir,
		listenPort:         listenPort,
		transport:          transport,
		caCert:             caCert,
		caKey:              caKey,
		caPEM:              caPEM,
		certCache:          make(map[string]*tls.Certificate),
		policiesBySourceIP: make(map[string]sourcePolicy),
		sourceIPByInstance: make(map[string]string),
	}, nil
}

func (s *Service) EnsureStarted(ctx context.Context, gatewayIP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener != nil {
		if gatewayIP != "" && gatewayIP != s.gatewayIP {
			return fmt.Errorf("%w: current=%s requested=%s", ErrGatewayMismatch, s.gatewayIP, gatewayIP)
		}
		return nil
	}

	s.gatewayIP = gatewayIP
	listenAddr := net.JoinHostPort(gatewayIP, strconv.Itoa(s.listenPort))
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen egress proxy on %s: %w", listenAddr, err)
	}

	s.listener = ln
	s.server = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		if serveErr := s.server.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			slog.Error("egress proxy server exited", "error", serveErr)
		}
	}()

	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == nil {
		return nil
	}
	err := s.server.Shutdown(ctx)
	s.server = nil
	s.listener = nil
	return err
}

func (s *Service) RegisterInstance(ctx context.Context, gatewayIP string, cfg InstanceConfig) (GuestConfig, error) {
	if err := s.EnsureStarted(ctx, gatewayIP); err != nil {
		return GuestConfig{}, err
	}

	if err := applyEgressEnforcement(cfg.InstanceID, cfg.TAPDevice, gatewayIP, s.listenPort); err != nil {
		return GuestConfig{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if prevIP, ok := s.sourceIPByInstance[cfg.InstanceID]; ok {
		delete(s.policiesBySourceIP, prevIP)
	}

	policyMap := make(map[string]string, len(cfg.MockToRealEnvVar))
	for mock, envVar := range cfg.MockToRealEnvVar {
		policyMap[mock] = envVar
	}

	s.sourceIPByInstance[cfg.InstanceID] = cfg.SourceIP
	s.policiesBySourceIP[cfg.SourceIP] = sourcePolicy{MockToRealEnvVar: policyMap}

	return GuestConfig{
		Enabled:   true,
		ProxyURL:  s.proxyURLLocked(),
		CACertPEM: s.caPEM,
	}, nil
}

func (s *Service) UnregisterInstance(_ context.Context, instanceID string) {
	s.mu.Lock()
	sourceIP, ok := s.sourceIPByInstance[instanceID]
	if ok {
		delete(s.sourceIPByInstance, instanceID)
		delete(s.policiesBySourceIP, sourceIP)
	}
	s.mu.Unlock()

	_ = removeEgressEnforcement(instanceID)
}

func (s *Service) ProxyURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxyURLLocked()
}

func (s *Service) proxyURLLocked() string {
	if s.gatewayIP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", s.gatewayIP, s.listenPort)
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sourceIP := sourceIPFromRemoteAddr(r.RemoteAddr)
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r, sourceIP)
		return
	}
	s.handleHTTPProxyRequest(w, r, sourceIP, false)
}

func (s *Service) handleHTTPProxyRequest(w http.ResponseWriter, r *http.Request, sourceIP string, insideTunnel bool) {
	outReq := r.Clone(r.Context())
	outReq.Header = cloneHeader(r.Header)
	removeHopByHopHeaders(outReq.Header)

	if !insideTunnel {
		if outReq.URL == nil || !outReq.URL.IsAbs() {
			outReq.URL = &url.URL{
				Scheme:   "http",
				Host:     r.Host,
				Path:     r.URL.Path,
				RawPath:  r.URL.RawPath,
				RawQuery: r.URL.RawQuery,
			}
		}
	}

	outReq.RequestURI = ""
	s.applyHeaderReplacements(sourceIP, outReq.Header)

	resp, err := s.transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("proxy upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	removeHopByHopHeaders(resp.Header)
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Service) handleConnect(w http.ResponseWriter, r *http.Request, sourceIP string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack failed: %v", err), http.StatusServiceUnavailable)
		return
	}
	defer clientConn.Close()

	_, _ = io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	targetHost := normalizeHost(r.Host)
	cert, err := s.getOrCreateLeafCert(targetHost)
	if err != nil {
		return
	}

	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*cert},
	})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	defer tlsConn.Close()

	reader := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			if err == io.EOF {
				return
			}
			return
		}

		if req.URL == nil {
			req.URL = &url.URL{}
		}
		if req.Host == "" {
			req.Host = r.Host
		}
		req.URL.Scheme = "https"
		req.URL.Host = req.Host
		req.RequestURI = ""
		req.Header = cloneHeader(req.Header)
		removeHopByHopHeaders(req.Header)
		s.applyHeaderReplacements(sourceIP, req.Header)

		resp, err := s.transport.RoundTrip(req)
		if err != nil {
			_, _ = io.WriteString(tlsConn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}

		removeHopByHopHeaders(resp.Header)
		if err := resp.Write(tlsConn); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()

		if req.Close || resp.Close {
			return
		}
	}
}

func (s *Service) getOrCreateLeafCert(host string) (*tls.Certificate, error) {
	s.mu.RLock()
	cached := s.certCache[host]
	s.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	cert, err := signHostCertificate(s.caCert, s.caKey, host)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.certCache[host]; existing != nil {
		return existing, nil
	}
	s.certCache[host] = cert
	return cert, nil
}

func (s *Service) applyHeaderReplacements(sourceIP string, headers http.Header) {
	replacements := s.resolveReplacements(sourceIP)
	if len(replacements) == 0 {
		return
	}

	for key, vals := range headers {
		for i := range vals {
			updated := vals[i]
			for mock, real := range replacements {
				updated = strings.ReplaceAll(updated, mock, real)
			}
			vals[i] = updated
		}
		headers[key] = vals
	}
}

func (s *Service) resolveReplacements(sourceIP string) map[string]string {
	s.mu.RLock()
	policy, ok := s.policiesBySourceIP[sourceIP]
	s.mu.RUnlock()
	if !ok {
		return nil
	}

	resolved := make(map[string]string, len(policy.MockToRealEnvVar))
	for mock, envVar := range policy.MockToRealEnvVar {
		if mock == "" || envVar == "" {
			continue
		}
		real, ok := os.LookupEnv(envVar)
		if !ok || real == "" {
			continue
		}
		resolved[mock] = real
	}
	return resolved
}

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopByHopHeaders(h http.Header) {
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vv := range src {
		copied := make([]string, len(vv))
		copy(copied, vv)
		dst[k] = copied
	}
	return dst
}

func sourceIPFromRemoteAddr(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
