package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
)

// PortForwardHandler forwards HTTP and WebSocket requests on
// /instances/{id}/ports/{port}/* to that TCP port on a macOS guest's loopback
// interface. The Authorization header, hypeman's only credential, is not
// forwarded. Responses come back unchanged; rewriting guest URLs is the caller's job.
// There is no response timeout, so long polls work; a client disconnect ends the forward.
func (s *ApiService) PortForwardHandler(w http.ResponseWriter, r *http.Request) {
	inst := mw.GetResolvedInstance[instances.Instance](r.Context())
	if inst == nil {
		http.Error(w, `{"code":"internal_error","message":"resource not resolved"}`, http.StatusInternalServerError)
		return
	}
	if reason := inst.GuestOperationUnsupportedReason(instances.GuestOpPortForward); reason != "" {
		writeGuestUnsupported(w, reason)
		return
	}
	port, err := strconv.ParseUint(chi.URLParam(r, "port"), 10, 16)
	if err != nil || port == 0 {
		http.Error(w, `{"code":"invalid_port","message":"port must be between 1 and 65535"}`, http.StatusBadRequest)
		return
	}
	if inst.State != instances.StateRunning {
		http.Error(w, fmt.Sprintf(`{"code":"invalid_state","message":"instance must be running (current state: %s)"}`, inst.State), http.StatusConflict)
		return
	}
	path, rawPath, err := forwardedPath(r)
	if err != nil {
		http.Error(w, `{"code":"invalid_path","message":"invalid escaped path"}`, http.StatusBadRequest)
		return
	}
	dialer, err := s.InstanceManager.GetVsockDialer(r.Context(), inst.Id)
	if err != nil {
		http.Error(w, `{"code":"unavailable","message":"guest agent unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	target := net.JoinHostPort("127.0.0.1", strconv.FormatUint(port, 10))
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target
			pr.Out.URL.Path = path
			pr.Out.URL.RawPath = rawPath
			// Guest services such as Chrome DevTools only accept a loopback Host.
			pr.Out.Host = target
			pr.Out.Header.Del("Authorization")
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return guest.DialPort(ctx, dialer, uint16(port))
			},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.FromContext(r.Context()).WarnContext(r.Context(), "guest port forward failed", "instance_id", inst.Id, "port", port, "error", err)
			if errors.Is(err, guest.ErrGuestPortUnavailable) {
				http.Error(w, `{"code":"port_unavailable","message":"nothing is listening on the guest port"}`, http.StatusBadGateway)
				return
			}
			if errors.Is(err, guest.ErrPortForwarderUnavailable) {
				http.Error(w, `{"code":"unavailable","message":"guest agent unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			http.Error(w, `{"code":"bad_gateway","message":"guest port forward failed"}`, http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// forwardedPath returns the guest path for the wildcard after /ports/{port}. chi
// matches on the escaped path when the request has one, so the wildcard keeps its
// escaping (for example %2F) and is unescaped here rather than escaped twice.
func forwardedPath(r *http.Request) (path, rawPath string, err error) {
	suffix := chi.URLParam(r, "*")
	if r.URL.RawPath == "" {
		return "/" + suffix, "", nil
	}
	unescaped, err := url.PathUnescape(suffix)
	if err != nil {
		return "", "", err
	}
	return "/" + unescaped, "/" + suffix, nil
}
