package registrypush

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/kernel/hypeman/lib/images"
)

// ErrUnauthorized means the destination registry rejected the push
// credentials. On pull a 401 is treated as "image not visible"; on push it is
// a caller-fixable credential problem and must surface as such.
var ErrUnauthorized = errors.New("registry authentication failed")

// classifyPushError maps a push failure into a typed hypeman error.
// Authentication failures become ErrUnauthorized; everything else falls
// through to images.ClassifyRegistryError (rate limits, missing repos, ...).
func classifyPushError(err error) error {
	var terr *transport.Error
	if errors.As(err, &terr) {
		if terr.StatusCode == http.StatusUnauthorized || terr.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w: %v", ErrUnauthorized, err)
		}
		for _, diag := range terr.Errors {
			switch strings.ToLower(string(diag.Code)) {
			case "unauthorized", "denied":
				return fmt.Errorf("%w: %v", ErrUnauthorized, err)
			}
		}
	}
	return images.ClassifyRegistryError(err)
}
