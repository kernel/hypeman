// Package dns provides a local DNS server for dynamic instance resolution.
// It enables Caddy to resolve instance names to IP addresses at request time.
package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/miekg/dns"
)

const (
	// DefaultPort is the default port for the local DNS server.
	DefaultPort = 5353

	// Suffix is the domain suffix used for instance resolution.
	// Queries like "my-instance.hypeman.internal" will be resolved.
	Suffix = "hypeman.internal"

	// DefaultTTL is the TTL for DNS responses in seconds.
	// Keep it low since instance IPs can change.
	DefaultTTL = 5
)

// InstanceResolver provides instance IP resolution.
// This interface is implemented by the instances package.
type InstanceResolver interface {
	// ResolveInstanceIP resolves an instance name or ID to its IP address.
	ResolveInstanceIP(ctx context.Context, nameOrID string) (string, error)
}

// Server provides DNS-based instance resolution for Caddy.
// It listens on a local port and responds to A record queries
// for instances in the form "<instance>.hypeman.internal".
type Server struct {
	resolver InstanceResolver
	port     int
	server   *dns.Server
	log      *slog.Logger
	mu       sync.Mutex
	running  bool
}

// NewServer creates a new DNS server for instance resolution.
// If port is 0, the OS will assign a random available port.
// The actual port can be retrieved with Port() after Start() is called.
func NewServer(resolver InstanceResolver, port int, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		resolver: resolver,
		port:     port,
		log:      log,
	}
}

// Start starts the DNS server.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}

	// Create DNS handler
	mux := dns.NewServeMux()
	mux.HandleFunc(Suffix+".", s.handleQuery)

	// Bind to UDP socket first to get actual port (important when port is 0)
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("bind DNS server: %w", err)
	}

	// Update port to actual assigned port (useful when s.port was 0)
	s.port = conn.LocalAddr().(*net.UDPAddr).Port

	// Create UDP server with pre-bound connection
	s.server = &dns.Server{
		PacketConn: conn,
		Handler:    mux,
	}

	// Start server in background
	go func() {
		s.log.Info("Starting DNS server for instance resolution", "addr", conn.LocalAddr().String(), "suffix", Suffix)
		if err := s.server.ActivateAndServe(); err != nil {
			s.log.Error("DNS server error", "error", err)
		}
	}()

	s.running = true
	return nil
}

// Stop stops the DNS server.
func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running || s.server == nil {
		return nil
	}

	err := s.server.Shutdown()
	s.running = false
	return err
}

// Port returns the port the DNS server is listening on.
func (s *Server) Port() int {
	return s.port
}

// IsRunning returns true if the DNS server is running.
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// handleQuery handles incoming DNS queries.
func (s *Server) handleQuery(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	for _, q := range r.Question {
		switch q.Qtype {
		case dns.TypeA:
			s.handleAQuery(m, q)
		case dns.TypeAAAA:
			// We don't support IPv6 for instances, return empty
			// This prevents Caddy from waiting for AAAA responses
		default:
			// Unsupported query type
		}
	}

	w.WriteMsg(m)
}

// handleAQuery handles A record queries.
func (s *Server) handleAQuery(m *dns.Msg, q dns.Question) {
	// Parse instance name from query
	// Query format: "<instance>.hypeman.internal."
	name := strings.TrimSuffix(q.Name, ".")
	suffix := "." + Suffix
	if !strings.HasSuffix(name, suffix) {
		s.log.Debug("DNS query doesn't match suffix", "name", name, "suffix", suffix)
		return
	}

	instanceName := strings.TrimSuffix(name, suffix)
	if instanceName == "" {
		s.log.Debug("DNS query has empty instance name", "name", name)
		return
	}

	// Resolve instance IP
	ctx := context.Background()
	ip, err := s.resolver.ResolveInstanceIP(ctx, instanceName)
	if err != nil {
		s.log.Debug("DNS resolution failed", "instance", instanceName, "error", err)
		// Return NXDOMAIN by not adding any answer records
		m.Rcode = dns.RcodeNameError
		return
	}

	// Parse IP address
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		s.log.Error("Invalid IP from resolver", "instance", instanceName, "ip", ip)
		m.Rcode = dns.RcodeServerFailure
		return
	}

	// Only handle IPv4 for A records
	ipv4 := parsedIP.To4()
	if ipv4 == nil {
		s.log.Debug("Resolved IP is not IPv4", "instance", instanceName, "ip", ip)
		return
	}

	// Add A record to response
	rr := &dns.A{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    DefaultTTL,
		},
		A: ipv4,
	}
	m.Answer = append(m.Answer, rr)

	s.log.Debug("DNS query resolved", "instance", instanceName, "ip", ip)
}
