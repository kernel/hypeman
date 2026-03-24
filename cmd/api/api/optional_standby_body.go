package api

import (
	"io"
	"net/http"
	"strings"
)

// NormalizeOptionalStandbyBody rewrites empty standby POST bodies to "{}"
// so the generated strict handler can decode them without special casing.
func NormalizeOptionalStandbyBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		if isStandbyRoutePath(r.URL.Path) && requestBodyIsEmpty(r) {
			r.Body = io.NopCloser(strings.NewReader(`{}`))
			r.ContentLength = 2
			if r.Header.Get("Content-Type") == "" {
				r.Header.Set("Content-Type", "application/json")
			}
		}

		next.ServeHTTP(w, r)
	})
}

func isStandbyRoutePath(path string) bool {
	if !strings.HasPrefix(path, "/instances/") || !strings.HasSuffix(path, "/standby") {
		return false
	}

	instanceID := strings.TrimPrefix(path, "/instances/")
	instanceID = strings.TrimSuffix(instanceID, "/standby")
	return instanceID != "" && !strings.Contains(instanceID, "/")
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
