package egressproxy

import (
	"fmt"
	"net"
	"strings"
)

type domainMatcher struct {
	exact          string
	wildcardSuffix string
}

func normalizeDestinationHost(raw string) string {
	host := strings.TrimSpace(raw)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.TrimSuffix(host, ".")
	host = strings.ToLower(host)
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

// NormalizeAllowedDomainPattern validates and normalizes one allowlisted destination
// host pattern (exact host/IP or single-level wildcard *.example.com).
func NormalizeAllowedDomainPattern(pattern string) (string, error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return "", fmt.Errorf("allowed domain pattern must be non-empty")
	}
	if strings.Contains(p, "://") || strings.ContainsAny(p, "/?#") {
		return "", fmt.Errorf("allowed domain pattern %q must be a host pattern without scheme/path", p)
	}
	if _, _, err := net.SplitHostPort(p); err == nil {
		return "", fmt.Errorf("allowed domain pattern %q must not include a port", p)
	}

	if strings.HasPrefix(p, "*.") {
		suffixRaw := strings.TrimPrefix(p, "*.")
		if strings.Contains(suffixRaw, ":") {
			return "", fmt.Errorf("allowed wildcard domain pattern %q must not include a port", p)
		}

		base := normalizeDestinationHost(suffixRaw)
		if base == "" {
			return "", fmt.Errorf("allowed wildcard domain pattern %q has empty suffix", p)
		}
		if net.ParseIP(base) != nil {
			return "", fmt.Errorf("allowed wildcard domain pattern %q cannot target an IP", p)
		}
		if !isValidDNSName(base) {
			return "", fmt.Errorf("allowed wildcard domain pattern %q has invalid hostname suffix", p)
		}
		if strings.Count(base, ".") < 1 {
			return "", fmt.Errorf("allowed wildcard domain pattern %q must target at least a two-label domain", p)
		}
		return "*." + base, nil
	}

	if strings.Contains(p, "*") {
		return "", fmt.Errorf("allowed domain pattern %q has unsupported wildcard syntax", p)
	}

	host := normalizeDestinationHost(p)
	if host == "" {
		return "", fmt.Errorf("allowed domain pattern %q has empty host", p)
	}
	if net.ParseIP(host) != nil {
		return host, nil
	}
	if !isValidDNSName(host) {
		return "", fmt.Errorf("allowed domain pattern %q has invalid hostname", p)
	}
	return host, nil
}

func compileDomainMatchers(patterns []string) ([]domainMatcher, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	out := make([]domainMatcher, 0, len(patterns))
	for _, raw := range patterns {
		normalized, err := NormalizeAllowedDomainPattern(raw)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(normalized, "*.") {
			out = append(out, domainMatcher{wildcardSuffix: "." + strings.TrimPrefix(normalized, "*.")})
			continue
		}
		out = append(out, domainMatcher{exact: normalized})
	}
	return out, nil
}

func matchesAnyDomain(host string, matchers []domainMatcher) bool {
	if len(matchers) == 0 {
		return true
	}
	for _, m := range matchers {
		if m.matches(host) {
			return true
		}
	}
	return false
}

func (m domainMatcher) matches(host string) bool {
	if host == "" {
		return false
	}
	if m.exact != "" {
		return host == m.exact
	}
	if m.wildcardSuffix == "" || !strings.HasSuffix(host, m.wildcardSuffix) {
		return false
	}
	prefix := strings.TrimSuffix(host, m.wildcardSuffix)
	return prefix != "" && !strings.Contains(prefix, ".")
}

func isValidDNSName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, ch := range label {
			isLetter := ch >= 'a' && ch <= 'z'
			isDigit := ch >= '0' && ch <= '9'
			isHyphen := ch == '-'
			if !(isLetter || isDigit || isHyphen) {
				return false
			}
			if (i == 0 || i == len(label)-1) && isHyphen {
				return false
			}
		}
	}
	return true
}
