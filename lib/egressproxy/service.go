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
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type sourcePolicy struct {
	headerInjectRules []headerInjectRule
}

type headerInjectRule struct {
	headerName     string
	headerValue    string
	domainMatchers []domainMatcher
}

const defaultLeafCertCacheLimit = 512

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

	certCache      map[string]*tls.Certificate
	certCacheOrder []string
	certCacheLimit int

	policiesBySourceIP map[string]sourcePolicy
	sourceIPByInstance map[string]string
	metrics            *metrics
	tracer             trace.Tracer
	log                *slog.Logger
}

func NewService(dataDir string, listenPort int) (*Service, error) {
	return NewServiceWithOptions(dataDir, listenPort, ServiceOptions{})
}

func NewServiceWithOptions(dataDir string, listenPort int, opts ServiceOptions) (*Service, error) {
	if listenPort <= 0 {
		listenPort = DefaultListenPort
	}

	caCert, caKey, caPEM, err := loadOrCreateCA(dataDir)
	if err != nil {
		return nil, err
	}

	rootCAs, err := buildRootCAPool(opts)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	dialContext := publicDialContext(net.DefaultResolver, dialer.DialContext)
	if opts.DialContext != nil {
		dialContext = opts.DialContext
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         dialContext,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		TLSClientConfig: &tls.Config{
			RootCAs: rootCAs,
		},
	}

	log := opts.Logger
	if log == nil {
		log = logger.NewSubsystemLogger(logger.SubsystemEgress, logger.NewConfig(), nil)
	}

	svc := &Service{
		dataDir:            dataDir,
		listenPort:         listenPort,
		transport:          transport,
		caCert:             caCert,
		caKey:              caKey,
		caPEM:              caPEM,
		certCache:          make(map[string]*tls.Certificate),
		certCacheLimit:     defaultLeafCertCacheLimit,
		policiesBySourceIP: make(map[string]sourcePolicy),
		sourceIPByInstance: make(map[string]string),
		tracer:             opts.Tracer,
		log:                log,
	}
	if opts.Meter != nil {
		metrics, err := newMetrics(opts.Meter, svc)
		if err != nil {
			return nil, err
		}
		svc.metrics = metrics
	}

	return svc, nil
}

func buildRootCAPool(opts ServiceOptions) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	for _, pemData := range opts.AdditionalRootCAPEM {
		trimmed := strings.TrimSpace(pemData)
		if trimmed == "" {
			continue
		}
		if ok := pool.AppendCertsFromPEM([]byte(trimmed)); !ok {
			return nil, fmt.Errorf("append additional root CA PEM failed")
		}
	}
	return pool, nil
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
			s.log.ErrorContext(context.Background(), "egress proxy server exited", "error", serveErr)
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
	start := time.Now()
	log := s.loggerForContext(ctx)
	result := "error"
	var opErr error
	enforcementMode := enforcementModeLabel(cfg.BlockAllTCPEgress)
	ctx, span := s.startControlPlaneSpan(ctx, "EgressProxy.RegisterInstance",
		attribute.String("operation", "register"),
		attribute.String("enforcement_mode", enforcementMode),
		attribute.Bool("proxy_enabled", true),
		attribute.Int("inject_rule_count", len(cfg.HeaderInjectRules)),
	)
	defer func() {
		s.finishControlPlaneSpan(span, result, opErr)
		s.metrics.recordRegistration(ctx, "register", result, enforcementMode)
		s.metrics.recordControlPlaneDuration(ctx, "register", result, time.Since(start).Seconds())
	}()

	if err := s.EnsureStarted(ctx, gatewayIP); err != nil {
		log.WarnContext(ctx, "failed to ensure egress proxy service is started", "instance_id", cfg.InstanceID, "error", err)
		opErr = err
		return GuestConfig{}, err
	}

	if err := applyEgressEnforcement(cfg.InstanceID, cfg.SourceIP, cfg.TAPDevice, gatewayIP, s.listenPort, cfg.BlockAllTCPEgress); err != nil {
		log.WarnContext(ctx, "failed to apply egress proxy enforcement", "instance_id", cfg.InstanceID, "error", err)
		opErr = err
		return GuestConfig{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if prevIP, ok := s.sourceIPByInstance[cfg.InstanceID]; ok {
		delete(s.policiesBySourceIP, prevIP)
	}

	injectRules, err := compileHeaderInjectRules(cfg.HeaderInjectRules)
	if err != nil {
		log.WarnContext(ctx, "failed to compile egress proxy inject rules", "instance_id", cfg.InstanceID, "error", err)
		opErr = err
		return GuestConfig{}, err
	}

	s.sourceIPByInstance[cfg.InstanceID] = cfg.SourceIP
	s.policiesBySourceIP[cfg.SourceIP] = sourcePolicy{headerInjectRules: injectRules}

	result = "success"
	log.DebugContext(ctx, "registered instance with egress proxy", "instance_id", cfg.InstanceID, "enforcement_mode", enforcementMode, "inject_rule_count", len(injectRules))
	return GuestConfig{
		Enabled:   true,
		ProxyURL:  s.proxyURLLocked(),
		CACertPEM: s.caPEM,
	}, nil
}

func compileHeaderInjectRules(cfgRules []HeaderInjectRuleConfig) ([]headerInjectRule, error) {
	if len(cfgRules) == 0 {
		return nil, nil
	}
	out := make([]headerInjectRule, 0, len(cfgRules))
	for _, cfg := range cfgRules {
		if strings.TrimSpace(cfg.HeaderName) == "" || cfg.HeaderValue == "" {
			continue
		}
		matchers, err := compileDomainMatchers(cfg.AllowedDomains)
		if err != nil {
			return nil, err
		}
		out = append(out, headerInjectRule{
			headerName:     textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(cfg.HeaderName)),
			headerValue:    cfg.HeaderValue,
			domainMatchers: matchers,
		})
	}
	return out, nil
}

// UpdateInstanceRules replaces the header inject rules for a registered instance.
// Returns an error if the instance is not currently registered.
func (s *Service) UpdateInstanceRules(ctx context.Context, instanceID string, rules []HeaderInjectRuleConfig) error {
	start := time.Now()
	log := s.loggerForContext(ctx)
	result := "error"
	var opErr error
	ctx, span := s.startControlPlaneSpan(ctx, "EgressProxy.UpdateInstanceRules",
		attribute.String("operation", "update"),
		attribute.Bool("proxy_enabled", true),
		attribute.Int("inject_rule_count", len(rules)),
	)
	defer func() {
		s.finishControlPlaneSpan(span, result, opErr)
		s.metrics.recordRuleUpdate(ctx, result)
		s.metrics.recordControlPlaneDuration(ctx, "update", result, time.Since(start).Seconds())
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	sourceIP, ok := s.sourceIPByInstance[instanceID]
	if !ok {
		err := fmt.Errorf("instance %s is not registered with egress proxy", instanceID)
		log.WarnContext(ctx, "failed to update egress proxy rules", "instance_id", instanceID, "error", err)
		opErr = err
		return err
	}

	compiled, err := compileHeaderInjectRules(rules)
	if err != nil {
		err = fmt.Errorf("compile header inject rules: %w", err)
		log.WarnContext(ctx, "failed to compile egress proxy rules", "instance_id", instanceID, "error", err)
		opErr = err
		return err
	}

	s.policiesBySourceIP[sourceIP] = sourcePolicy{headerInjectRules: compiled}
	result = "success"
	log.DebugContext(ctx, "updated egress proxy rules", "instance_id", instanceID, "inject_rule_count", len(compiled))
	return nil
}

func (s *Service) UnregisterInstance(ctx context.Context, instanceID string) error {
	start := time.Now()
	log := s.loggerForContext(ctx)
	result := "error"
	var opErr error
	ctx, span := s.startControlPlaneSpan(ctx, "EgressProxy.UnregisterInstance",
		attribute.String("operation", "unregister"),
		attribute.Bool("proxy_enabled", true),
	)
	defer func() {
		s.finishControlPlaneSpan(span, result, opErr)
		s.metrics.recordRegistration(ctx, "unregister", result, "unknown")
		s.metrics.recordControlPlaneDuration(ctx, "unregister", result, time.Since(start).Seconds())
	}()

	s.mu.Lock()
	sourceIP, ok := s.sourceIPByInstance[instanceID]
	if ok {
		delete(s.sourceIPByInstance, instanceID)
		delete(s.policiesBySourceIP, sourceIP)
	}
	s.mu.Unlock()

	if err := removeEgressEnforcement(instanceID); err != nil {
		log.WarnContext(ctx, "failed to remove egress proxy enforcement", "instance_id", instanceID, "error", err)
		opErr = err
		return err
	}

	result = "success"
	log.DebugContext(ctx, "unregistered instance from egress proxy", "instance_id", instanceID)
	return nil
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
	if !s.isRegisteredSourceIP(sourceIP) {
		http.Error(w, "proxy source is not registered", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r, sourceIP)
		return
	}
	s.handleHTTPProxyRequest(w, r, sourceIP, false)
}

func (s *Service) isRegisteredSourceIP(sourceIP string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.policiesBySourceIP[sourceIP]
	return ok
}

func (s *Service) handleHTTPProxyRequest(w http.ResponseWriter, r *http.Request, sourceIP string, insideTunnel bool) {
	protocol := "http"
	if insideTunnel {
		protocol = "https"
	}
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
	destinationHost := normalizeDestinationHost(outReq.URL.Host)
	if destinationHost == "" {
		destinationHost = normalizeDestinationHost(outReq.Host)
	}
	injected := s.applyHeaderInjections(sourceIP, destinationHost, outReq.Header, false) > 0

	start := time.Now()
	resp, err := s.transport.RoundTrip(outReq)
	if err != nil {
		log := s.loggerForContext(r.Context())
		log.WarnContext(r.Context(), "egress proxy upstream request failed", "protocol", protocol, "injected", injected, "error", err)
		s.metrics.recordRequest(r.Context(), protocol, "upstream_error", injected)
		s.metrics.recordUpstreamDuration(r.Context(), protocol, "upstream_error", time.Since(start).Seconds())
		s.metrics.recordUpstreamFailure(r.Context(), protocol)
		http.Error(w, "proxy upstream error", http.StatusBadGateway)
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
	s.metrics.recordRequest(r.Context(), protocol, "success", injected)
	s.metrics.recordUpstreamDuration(r.Context(), protocol, "success", time.Since(start).Seconds())
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

	targetAuthority := strings.TrimSpace(r.Host)
	targetHost := normalizeDestinationHost(targetAuthority)
	if targetHost == "" {
		return
	}

	_, _ = io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	cert, err := s.getOrCreateLeafCert(targetHost)
	if err != nil {
		log := s.loggerForContext(r.Context())
		log.WarnContext(r.Context(), "egress proxy CONNECT setup failed", "stage", "leaf_cert", "destination_host", targetHost, "error", err)
		s.metrics.recordRequest(r.Context(), "https", "connect_cert_error", false)
		return
	}

	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*cert},
	})
	if err := tlsConn.Handshake(); err != nil {
		log := s.loggerForContext(r.Context())
		log.WarnContext(r.Context(), "egress proxy CONNECT setup failed", "stage", "client_tls_handshake", "destination_host", targetHost, "error", err)
		s.metrics.recordRequest(r.Context(), "https", "connect_handshake_error", false)
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
			req.Host = targetAuthority
		}
		req.URL.Scheme = "https"
		req.URL.Host = targetAuthority
		req.RequestURI = ""
		req.Header = cloneHeader(req.Header)
		removeHopByHopHeaders(req.Header)
		injected := s.applyHeaderInjections(sourceIP, targetHost, req.Header, true) > 0

		start := time.Now()
		resp, err := s.transport.RoundTrip(req)
		if err != nil {
			log := s.loggerForContext(r.Context())
			log.WarnContext(r.Context(), "egress proxy upstream request failed", "protocol", "https", "injected", injected, "error", err)
			s.metrics.recordRequest(r.Context(), "https", "upstream_error", injected)
			s.metrics.recordUpstreamDuration(r.Context(), "https", "upstream_error", time.Since(start).Seconds())
			s.metrics.recordUpstreamFailure(r.Context(), "https")
			_, _ = io.WriteString(tlsConn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}

		removeHopByHopHeaders(resp.Header)
		if err := resp.Write(tlsConn); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()
		s.metrics.recordRequest(r.Context(), "https", "success", injected)
		s.metrics.recordUpstreamDuration(r.Context(), "https", "success", time.Since(start).Seconds())

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
	if s.certCacheLimit > 0 && len(s.certCache) >= s.certCacheLimit && len(s.certCacheOrder) > 0 {
		evictHost := s.certCacheOrder[0]
		s.certCacheOrder = s.certCacheOrder[1:]
		delete(s.certCache, evictHost)
	}
	s.certCache[host] = cert
	s.certCacheOrder = append(s.certCacheOrder, host)
	return cert, nil
}

func (s *Service) applyHeaderInjections(sourceIP, destinationHost string, headers http.Header, isHTTPS bool) int {
	rules := s.resolveHeaderInjectRules(sourceIP, destinationHost, isHTTPS)
	if len(rules) == 0 {
		return 0
	}

	for _, rule := range rules {
		headers.Set(rule.headerName, rule.headerValue)
	}
	return len(rules)
}

func (s *Service) resolveHeaderInjectRules(sourceIP, destinationHost string, isHTTPS bool) []headerInjectRule {
	if !isHTTPS {
		return nil
	}

	host := normalizeDestinationHost(destinationHost)
	if host == "" {
		return nil
	}

	s.mu.RLock()
	policy, ok := s.policiesBySourceIP[sourceIP]
	s.mu.RUnlock()
	if !ok {
		return nil
	}

	resolved := make([]headerInjectRule, 0, len(policy.headerInjectRules))
	for _, rule := range policy.headerInjectRules {
		if rule.headerName == "" || rule.headerValue == "" {
			continue
		}
		if !matchesAnyDomain(host, rule.domainMatchers) {
			continue
		}
		resolved = append(resolved, rule)
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

func (s *Service) loggerForContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		log := logger.FromContext(ctx)
		if log != nil && log != slog.Default() {
			return log
		}
	}
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

func (s *Service) startControlPlaneSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if s.tracer == nil {
		return ctx, nil
	}
	ctx, span := s.tracer.Start(ctx, name)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	return ctx, span
}

func (s *Service) finishControlPlaneSpan(span trace.Span, result string, err error) {
	if span == nil {
		return
	}
	span.SetAttributes(attribute.String("result", result))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func enforcementModeLabel(blockAllTCPEgress bool) string {
	if blockAllTCPEgress {
		return "all"
	}
	return "http_https_only"
}
