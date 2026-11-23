package oidcfederation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GrantTypeJWTBearer is the RFC 7523 assertion grant.
const GrantTypeJWTBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// A URL-encoded assertion of the largest accepted size, with room for the other parameters.
const maxTokenRequestBytes = 3*MaxAssertionBytes + 4<<10

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	Scope            string `json:"scope"`
	Principal        string `json:"principal"`
	FederationRuleID string `json:"federation_rule_id"`
}

type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// ServeHTTP handles the assertion exchange:
//
//	POST /oauth/token
//	Content-Type: application/x-www-form-urlencoded
//
//	grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=<jwt>&federation_rule_id=<rule>
//
// The request carries no client authentication; the assertion is the only credential.
func (e *Exchanger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The server sets no read timeout and this route needs no credential, so
	// bound how long a caller may take to send the form. The deadline stays
	// in place when the request is rejected, because the server still reads
	// what is left of the body after the handler returns.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(e.readTimeout))
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "The request must be form-encoded")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTokenRequestBytes))
	if err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "The request is malformed or too large")
		return
	}
	// The whole body has arrived; the exchange below has its own timeout.
	_ = controller.SetReadDeadline(time.Time{})
	form, err := url.ParseQuery(string(body))
	if err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "The request is malformed or too large")
		return
	}
	for _, values := range form {
		if len(values) != 1 {
			writeTokenError(w, http.StatusBadRequest, "invalid_request", "Repeated parameter")
			return
		}
	}
	grantType := form.Get("grant_type")
	if grantType == "" {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: grant_type")
		return
	}
	if grantType != GrantTypeJWTBearer || !e.Enabled() {
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type", "Unsupported grant type")
		return
	}
	for key := range form {
		switch key {
		case "grant_type", "assertion", "federation_rule_id":
		default:
			writeTokenError(w, http.StatusBadRequest, "invalid_request", "Unsupported parameter")
			return
		}
	}
	if _, present := r.Header["Authorization"]; present {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "The assertion exchange does not accept client authentication")
		return
	}
	for _, required := range []string{"assertion", "federation_rule_id"} {
		if form.Get(required) == "" {
			writeTokenError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: "+required)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	token, err := e.Exchange(ctx, form.Get("federation_rule_id"), form.Get("assertion"))
	if err != nil {
		if errors.Is(err, ErrAssertion) {
			// One answer for every refusal: the caller learns nothing about the rule or the issuer.
			writeTokenError(w, http.StatusBadRequest, "invalid_grant", "The assertion was not accepted")
			return
		}
		writeTokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "The assertion could not be verified right now")
		return
	}
	writeTokenJSON(w, http.StatusOK, tokenResponse{
		AccessToken:      token.AccessToken,
		TokenType:        "Bearer",
		ExpiresIn:        token.ExpiresIn,
		Scope:            strings.Join(token.Permissions, " "),
		Principal:        token.Principal,
		FederationRuleID: token.Rule,
	})
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
