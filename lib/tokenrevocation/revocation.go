// Package tokenrevocation rejects specific API tokens before their expiry.
//
// Tokens are identified by the hex SHA-256 of the raw token string, so
// revoking a token does not require it to carry a jti claim and the
// configuration never holds a usable credential.
package tokenrevocation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// List is a set of revoked token hashes. A nil List revokes nothing.
type List struct {
	hashes map[string]struct{}
}

// New builds a List from hex-encoded SHA-256 token hashes.
func New(hashes []string) (*List, error) {
	l := &List{hashes: make(map[string]struct{}, len(hashes))}
	for i, h := range hashes {
		h = strings.ToLower(strings.TrimSpace(h))
		if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
			// Don't echo the value: a raw token pasted here by mistake would end up in logs.
			return nil, fmt.Errorf("entry %d is not a hex-encoded SHA-256 digest", i)
		}
		l.hashes[h] = struct{}{}
	}
	return l, nil
}

// Hash returns the hex-encoded SHA-256 of a raw token string.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IsRevoked reports whether the raw token string has been revoked.
func (l *List) IsRevoked(token string) bool {
	if l == nil || len(l.hashes) == 0 {
		return false
	}
	_, ok := l.hashes[Hash(token)]
	return ok
}

// Len returns the number of revoked token hashes.
func (l *List) Len() int {
	if l == nil {
		return 0
	}
	return len(l.hashes)
}
