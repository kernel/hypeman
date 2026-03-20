package oapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func decodeOptionalJSONBody[T any](r *http.Request, body *T) (*T, error) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}

	if err := json.NewDecoder(r.Body).Decode(body); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}

	return body, nil
}
