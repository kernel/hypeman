package authserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/kernel/hypeman/lib/oidcfederation"
)

// GrantTypeJWTBearer is the RFC 7523 assertion grant.
const GrantTypeJWTBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer"

const (
	tokenPath    = "/oauth/token"
	metadataPath = "/.well-known/oauth-authorization-server"
	keySetPath   = "/.well-known/jwks.json"

	// A URL-encoded identity token of the largest accepted size, with room for the other parameters.
	maxTokenRequestBytes = 3*oidcfederation.MaxAssertionBytes + 8<<10
	exchangeTimeout      = 20 * time.Second
)

// metadata is the RFC 8414 document, from which a resource server learns
// where the keys are.
func metadata(issuer string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"issuer":                                issuer,
		"token_endpoint":                        issuer + tokenPath,
		"jwks_uri":                              issuer + keySetPath,
		"grant_types_supported":                 []string{GrantTypeJWTBearer},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// Handler serves the token endpoint, the metadata and the key set.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+tokenPath, s.serveToken)
	mux.HandleFunc("GET "+metadataPath, public(s.metadata))
	mux.HandleFunc("GET "+keySetPath, public(s.keySet))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

// public serves a fixed JSON document that anyone may read. Nothing between
// here and a Hypeman server should hold on to it: a server decides for itself
// how long it keeps the key set.
func public(document []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(document)
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// serveToken handles the identity token exchange:
//
//	POST /oauth/token
//	Content-Type: application/x-www-form-urlencoded
//
//	grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=<jwt>&federation_rule_id=<rule>&resource=<audience>
//
// The request carries no client authentication; the identity token is the
// only credential. resource may be left out when the rule has one audience.
func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	// One deadline for reading the request and for the exchange, short of the
	// server's write deadline, so that an identity token is never spent for a
	// response that can no longer be written.
	ctx, cancel := context.WithTimeout(r.Context(), exchangeTimeout)
	defer cancel()
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "The request must be form-encoded")
		return
	}
	// Parameters belong in the body. One in the URL would be ignored, not obeyed.
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "Parameters must be sent in the request body")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTokenRequestBytes))
	if err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "The request is malformed or too large")
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "The request is malformed or too large")
		return
	}
	for key, values := range form {
		if len(values) != 1 {
			writeTokenError(w, http.StatusBadRequest, "invalid_request", "Repeated parameter")
			return
		}
		switch key {
		case "grant_type", "assertion", "federation_rule_id", "resource":
		default:
			writeTokenError(w, http.StatusBadRequest, "invalid_request", "Unsupported parameter")
			return
		}
	}
	if _, present := r.Header["Authorization"]; present {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "The identity token exchange does not accept client authentication")
		return
	}
	if form.Get("grant_type") == "" {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: grant_type")
		return
	}
	if form.Get("grant_type") != GrantTypeJWTBearer {
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type", "Unsupported grant type")
		return
	}
	for _, required := range []string{"assertion", "federation_rule_id"} {
		if form.Get(required) == "" {
			writeTokenError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: "+required)
			return
		}
	}
	token, err := s.Exchange(ctx, form.Get("federation_rule_id"), form.Get("assertion"), form.Get("resource"))
	switch {
	case errors.Is(err, ErrGrant):
		// One answer for every refusal: the caller learns nothing about the rule or the issuer.
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "The identity token was not accepted")
	case errors.Is(err, ErrTarget):
		writeTokenError(w, http.StatusBadRequest, "invalid_target", "The requested resource is not allowed")
	case err != nil:
		writeTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "A token cannot be issued right now")
	default:
		writeTokenJSON(w, http.StatusOK, tokenResponse{AccessToken: token.AccessToken, TokenType: "Bearer", ExpiresIn: token.ExpiresIn, Scope: token.Scope})
	}
}

func writeTokenError(w http.ResponseWriter, status int, code, description string) {
	writeTokenJSON(w, status, tokenError{Error: code, ErrorDescription: description})
}

func writeTokenJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
