// Package desktop contains transport-independent desktop guest interfaces.
package desktop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

const maxDiscoveryBytes = 2 << 20

// NewCDPProxy forwards discovery and WebSocket traffic to a fixed browser endpoint.
// The caller owns instance authorization, origin checks, guest capability admission,
// and the transport (normally a connection to the selected desktop agent over vsock).
// publicBase must be a trusted, instance-scoped ws/wss URL, not a request Host header.
// Requests must have the instance route prefix removed before reaching this handler.
func NewCDPProxy(transport http.RoundTripper, publicBase string) (http.Handler, error) {
	base, err := url.Parse(publicBase)
	if err != nil || base == nil || (base.Scheme != "ws" && base.Scheme != "wss") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.RawPath != "" || strings.HasSuffix(base.Path, "/") {
		return nil, fmt.Errorf("invalid public CDP base")
	}
	return newBrowserProxy(transport, base)
}

// NewCDPForwarder serves the guest's fixed loopback browser without interpreting
// discovery. The authenticated host proxy owns public URL validation/rewriting.
func NewCDPForwarder(transport http.RoundTripper) (http.Handler, error) {
	return newBrowserProxy(transport, nil)
}

func newBrowserProxy(transport http.RoundTripper, publicBase *url.URL) (http.Handler, error) {
	if transport == nil {
		return nil, fmt.Errorf("CDP transport required")
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme = "http"
			p.Out.URL.Host = "127.0.0.1:9222"
			p.Out.Host = "127.0.0.1:9222"
			p.Out.URL.RawQuery = ""
			p.Out.URL.User = nil
			p.Out.URL.Fragment = ""
			p.Out.URL.Opaque = ""
			// Do not forward API credentials or arbitrary application headers to the guest.
			headers := make(http.Header)
			for _, key := range []string{"Accept", "Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions"} {
				for _, value := range p.Out.Header.Values(key) {
					headers.Add(key, value)
				}
			}
			p.Out.Header = headers
		},
		ModifyResponse: func(r *http.Response) error {
			r.Header.Del("Set-Cookie")
			if r.StatusCode == http.StatusSwitchingProtocols && !debuggerPath(r.Request.URL.Path) {
				return fmt.Errorf("unexpected discovery upgrade")
			}
			if r.StatusCode >= 300 && r.StatusCode < 400 {
				return fmt.Errorf("CDP redirects unsupported")
			}
			if publicBase != nil && r.StatusCode == http.StatusOK && discoveryPath(r.Request.URL.Path) {
				return rewriteDiscovery(r, publicBase.String())
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "browser unavailable or invalid discovery response", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := ValidateCDPRequest(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		proxy.ServeHTTP(w, r)
	}), nil
}

// ValidateCDPRequest allows API admission to run before dialing any guest service.
func ValidateCDPRequest(r *http.Request) error {
	if ValidateBodylessRequest(r) != nil || r.Method != http.MethodGet || !(discoveryPath(r.URL.Path) || debuggerPath(r.URL.Path)) {
		return fmt.Errorf("unsupported CDP request")
	}
	if debuggerPath(r.URL.Path) && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return fmt.Errorf("WebSocket upgrade required")
	}
	return nil
}

// ValidateBodylessRequest rejects alternate URL encodings, query parameters and
// request bodies for both desktop control and CDP routes.
func ValidateBodylessRequest(r *http.Request) error {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" {
		return fmt.Errorf("invalid desktop request")
	}
	return nil
}

func discoveryPath(path string) bool {
	switch path {
	case "/json", "/json/list", "/json/version", "/json/protocol":
		return true
	}
	return false
}

func debuggerPath(path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "devtools" || (parts[2] != "browser" && parts[2] != "page") || parts[3] == "" {
		return false
	}
	for _, c := range parts[3] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func rewriteDiscovery(r *http.Response, base string) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxDiscoveryBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxDiscoveryBytes {
		return fmt.Errorf("CDP discovery too large")
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	rewrite := func(v any) error {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid CDP discovery object")
		}
		// These frontend links can embed an unproxied debugger address. Clients use the
		// rewritten webSocketDebuggerUrl; hosting a DevTools frontend is separate.
		delete(obj, "devtoolsFrontendUrl")
		delete(obj, "devtoolsFrontendUrlCompat")
		raw, exists := obj["webSocketDebuggerUrl"]
		if !exists {
			return nil
		}
		address, ok := raw.(string)
		if !ok {
			return fmt.Errorf("invalid debugger URL")
		}
		u, err := url.Parse(address)
		if err != nil || u.Scheme != "ws" || u.Host != "127.0.0.1:9222" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !debuggerPath(u.Path) {
			return fmt.Errorf("unexpected debugger URL")
		}
		obj["webSocketDebuggerUrl"] = base + u.Path
		return nil
	}
	switch v := value.(type) {
	case []any:
		for _, obj := range v {
			if err := rewrite(obj); err != nil {
				return err
			}
		}
	default:
		if err := rewrite(v); err != nil {
			return err
		}
	}
	data, err = json.Marshal(value)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.Header.Del("Content-Length")
	r.Header.Del("Content-Encoding")
	r.Header.Del("ETag")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cache-Control", "no-store")
	return nil
}
