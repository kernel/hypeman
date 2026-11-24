package oidcfederation

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func pointerParts(pointer string) ([]string, error) {
	if pointer == "" || !strings.HasPrefix(pointer, "/") || len(pointer) > 1024 {
		return nil, fmt.Errorf("%w: claim selector must be a JSON Pointer", ErrInvalid)
	}
	parts := strings.Split(pointer[1:], "/")
	for i, part := range parts {
		var decoded strings.Builder
		for j := 0; j < len(part); j++ {
			if part[j] != '~' {
				decoded.WriteByte(part[j])
				continue
			}
			j++
			if j == len(part) || (part[j] != '0' && part[j] != '1') {
				return nil, ErrInvalid
			}
			if part[j] == '0' {
				decoded.WriteByte('~')
			} else {
				decoded.WriteByte('/')
			}
		}
		parts[i] = decoded.String()
	}
	return parts, nil
}

// ClaimString resolves a JSON Pointer to a scalar claim as text. Providers send
// some claims as booleans or numbers (Google's email_verified is true, not
// "true"), so those compare by their JSON text. Objects and arrays never match.
func ClaimString(claims map[string]any, pointer string) (string, bool) {
	parts, err := pointerParts(pointer)
	if err != nil {
		return "", false
	}
	var value any = claims
	for _, part := range parts {
		switch node := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = node[part]
			if !ok {
				return "", false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) || strconv.Itoa(index) != part {
				return "", false
			}
			value = node[index]
		default:
			return "", false
		}
	}
	switch scalar := value.(type) {
	case string:
		return scalar, true
	case bool:
		return strconv.FormatBool(scalar), true
	case json.Number:
		return scalar.String(), true
	case float64:
		return strconv.FormatFloat(scalar, 'f', -1, 64), true
	default:
		return "", false
	}
}
