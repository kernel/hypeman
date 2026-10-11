package api

import (
	"encoding/json"
	"net/http"
)

// writeGuestUnsupported writes the shared 501 "unsupported" body for raw HTTP handlers.
func writeGuestUnsupported(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": "unsupported", "message": reason})
}
