package api

import (
	"io"
	"net/http"
	"strings"
)

// optionalBodyRoutes are the instance actions whose POST body is optional.
var optionalBodyRoutes = []string{"/standby", "/stop"}

// NormalizeOptionalBody rewrites empty POST bodies of instance actions with an
// optional body to "{}" so the generated strict handler can decode them without
// special casing.
func NormalizeOptionalBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		if isOptionalBodyRoutePath(r.URL.Path) && requestBodyIsEmpty(r) {
			r.Body = io.NopCloser(strings.NewReader(`{}`))
			r.ContentLength = 2
			if r.Header.Get("Content-Type") == "" {
				r.Header.Set("Content-Type", "application/json")
			}
		}

		next.ServeHTTP(w, r)
	})
}

func isOptionalBodyRoutePath(path string) bool {
	if !strings.HasPrefix(path, "/instances/") {
		return false
	}
	for _, action := range optionalBodyRoutes {
		if !strings.HasSuffix(path, action) {
			continue
		}
		instanceID := strings.TrimSuffix(strings.TrimPrefix(path, "/instances/"), action)
		return instanceID != "" && !strings.Contains(instanceID, "/")
	}
	return false
}

func requestBodyIsEmpty(r *http.Request) bool {
	if r == nil {
		return true
	}
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	return r.ContentLength == 0 && len(r.TransferEncoding) == 0
}
