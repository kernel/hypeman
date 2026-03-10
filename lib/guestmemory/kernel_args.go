package guestmemory

import "strings"

// MergeKernelArgs merges kernel args deterministically.
// Duplicate keys are de-duplicated with "last write wins" semantics.
func MergeKernelArgs(base string, extras ...string) string {
	tokens := strings.Fields(base)
	order := make([]string, 0, len(tokens))
	values := make(map[string]string, len(tokens))

	for _, tok := range tokens {
		k := argKey(tok)
		if _, ok := values[k]; !ok {
			order = append(order, k)
		}
		values[k] = tok
	}

	for _, extra := range extras {
		for _, tok := range strings.Fields(extra) {
			k := argKey(tok)
			if _, ok := values[k]; !ok {
				order = append(order, k)
			}
			values[k] = tok
		}
	}

	merged := make([]string, 0, len(order))
	for _, k := range order {
		merged = append(merged, values[k])
	}
	return strings.Join(merged, " ")
}

func argKey(token string) string {
	if idx := strings.IndexByte(token, '='); idx >= 0 {
		return token[:idx]
	}
	return token
}
