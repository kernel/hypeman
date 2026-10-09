package desktop

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseOrigin validates an administrator-provided API origin. Forwarded headers
// and request Host headers are deliberately not involved in URL generation.
func ParseOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("desktop API origin must be an http(s) origin without credentials, query or path")
	}
	u.Path = ""
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func SameOrigin(raw string, expected *url.URL) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == expected.Scheme && strings.EqualFold(u.Host, expected.Host) && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == ""
}
