// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package hypeman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/onkernel/hypeman-go/internal/apijson"
	"github.com/onkernel/hypeman-go/internal/requestconfig"
	"github.com/onkernel/hypeman-go/option"
	"github.com/onkernel/hypeman-go/packages/param"
	"github.com/onkernel/hypeman-go/packages/respjson"
)

// IngressService contains methods and other services that help with interacting
// with the hypeman API.
//
// Note, unlike clients, this service does not read variables from the environment
// automatically. You should not instantiate this service directly, and instead use
// the [NewIngressService] method instead.
type IngressService struct {
	Options []option.RequestOption
}

// NewIngressService generates a new service that applies the given options to each
// request. These options are applied after the parent client's options (if there
// is one), and before any request-specific options.
func NewIngressService(opts ...option.RequestOption) (r IngressService) {
	r = IngressService{}
	r.Options = opts
	return
}

// Create ingress
func (r *IngressService) New(ctx context.Context, body IngressNewParams, opts ...option.RequestOption) (res *Ingress, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "ingresses"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodPost, path, body, &res, opts...)
	return
}

// List ingresses
func (r *IngressService) List(ctx context.Context, opts ...option.RequestOption) (res *[]Ingress, err error) {
	opts = slices.Concat(r.Options, opts)
	path := "ingresses"
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

// Delete ingress
func (r *IngressService) Delete(ctx context.Context, id string, opts ...option.RequestOption) (err error) {
	opts = slices.Concat(r.Options, opts)
	opts = append([]option.RequestOption{option.WithHeader("Accept", "*/*")}, opts...)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("ingresses/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodDelete, path, nil, nil, opts...)
	return
}

// Get ingress details
func (r *IngressService) Get(ctx context.Context, id string, opts ...option.RequestOption) (res *Ingress, err error) {
	opts = slices.Concat(r.Options, opts)
	if id == "" {
		err = errors.New("missing required id parameter")
		return
	}
	path := fmt.Sprintf("ingresses/%s", id)
	err = requestconfig.ExecuteNewRequest(ctx, http.MethodGet, path, nil, &res, opts...)
	return
}

type Ingress struct {
	// Auto-generated unique identifier
	ID string `json:"id,required"`
	// Creation timestamp (RFC3339)
	CreatedAt time.Time `json:"created_at,required" format:"date-time"`
	// Human-readable name
	Name string `json:"name,required"`
	// Routing rules for this ingress
	Rules []IngressRule `json:"rules,required"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		ID          respjson.Field
		CreatedAt   respjson.Field
		Name        respjson.Field
		Rules       respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r Ingress) RawJSON() string { return r.JSON.raw }
func (r *Ingress) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type IngressMatch struct {
	// Hostname to match. Can be:
	//
	// - Literal: "api.example.com" (exact match on Host header)
	// - Pattern: "{instance}.example.com" (dynamic routing based on subdomain)
	//
	// Pattern hostnames use named captures in curly braces (e.g., {instance}, {app})
	// that extract parts of the hostname for routing. The extracted values can be
	// referenced in the target.instance field.
	Hostname string `json:"hostname,required"`
	// Host port to listen on for this rule (default 80)
	Port int64 `json:"port"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Hostname    respjson.Field
		Port        respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r IngressMatch) RawJSON() string { return r.JSON.raw }
func (r *IngressMatch) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// ToParam converts this IngressMatch to a IngressMatchParam.
//
// Warning: the fields of the param type will not be present. ToParam should only
// be used at the last possible moment before sending a request. Test for this with
// IngressMatchParam.Overrides()
func (r IngressMatch) ToParam() IngressMatchParam {
	return param.Override[IngressMatchParam](json.RawMessage(r.RawJSON()))
}

// The property Hostname is required.
type IngressMatchParam struct {
	// Hostname to match. Can be:
	//
	// - Literal: "api.example.com" (exact match on Host header)
	// - Pattern: "{instance}.example.com" (dynamic routing based on subdomain)
	//
	// Pattern hostnames use named captures in curly braces (e.g., {instance}, {app})
	// that extract parts of the hostname for routing. The extracted values can be
	// referenced in the target.instance field.
	Hostname string `json:"hostname,required"`
	// Host port to listen on for this rule (default 80)
	Port param.Opt[int64] `json:"port,omitzero"`
	paramObj
}

func (r IngressMatchParam) MarshalJSON() (data []byte, err error) {
	type shadow IngressMatchParam
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *IngressMatchParam) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type IngressRule struct {
	Match  IngressMatch  `json:"match,required"`
	Target IngressTarget `json:"target,required"`
	// Auto-create HTTP to HTTPS redirect for this hostname (only applies when tls is
	// enabled)
	RedirectHTTP bool `json:"redirect_http"`
	// Enable TLS termination (certificate auto-issued via ACME).
	Tls bool `json:"tls"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Match        respjson.Field
		Target       respjson.Field
		RedirectHTTP respjson.Field
		Tls          respjson.Field
		ExtraFields  map[string]respjson.Field
		raw          string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r IngressRule) RawJSON() string { return r.JSON.raw }
func (r *IngressRule) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// ToParam converts this IngressRule to a IngressRuleParam.
//
// Warning: the fields of the param type will not be present. ToParam should only
// be used at the last possible moment before sending a request. Test for this with
// IngressRuleParam.Overrides()
func (r IngressRule) ToParam() IngressRuleParam {
	return param.Override[IngressRuleParam](json.RawMessage(r.RawJSON()))
}

// The properties Match, Target are required.
type IngressRuleParam struct {
	Match  IngressMatchParam  `json:"match,omitzero,required"`
	Target IngressTargetParam `json:"target,omitzero,required"`
	// Auto-create HTTP to HTTPS redirect for this hostname (only applies when tls is
	// enabled)
	RedirectHTTP param.Opt[bool] `json:"redirect_http,omitzero"`
	// Enable TLS termination (certificate auto-issued via ACME).
	Tls param.Opt[bool] `json:"tls,omitzero"`
	paramObj
}

func (r IngressRuleParam) MarshalJSON() (data []byte, err error) {
	type shadow IngressRuleParam
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *IngressRuleParam) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type IngressTarget struct {
	// Target instance name, ID, or capture reference.
	//
	//   - For literal hostnames: Use the instance name or ID directly (e.g., "my-api")
	//   - For pattern hostnames: Reference a capture from the hostname (e.g.,
	//     "{instance}")
	//
	// When using pattern hostnames, the instance is resolved dynamically at request
	// time.
	Instance string `json:"instance,required"`
	// Target port on the instance
	Port int64 `json:"port,required"`
	// JSON contains metadata for fields, check presence with [respjson.Field.Valid].
	JSON struct {
		Instance    respjson.Field
		Port        respjson.Field
		ExtraFields map[string]respjson.Field
		raw         string
	} `json:"-"`
}

// Returns the unmodified JSON received from the API
func (r IngressTarget) RawJSON() string { return r.JSON.raw }
func (r *IngressTarget) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

// ToParam converts this IngressTarget to a IngressTargetParam.
//
// Warning: the fields of the param type will not be present. ToParam should only
// be used at the last possible moment before sending a request. Test for this with
// IngressTargetParam.Overrides()
func (r IngressTarget) ToParam() IngressTargetParam {
	return param.Override[IngressTargetParam](json.RawMessage(r.RawJSON()))
}

// The properties Instance, Port are required.
type IngressTargetParam struct {
	// Target instance name, ID, or capture reference.
	//
	//   - For literal hostnames: Use the instance name or ID directly (e.g., "my-api")
	//   - For pattern hostnames: Reference a capture from the hostname (e.g.,
	//     "{instance}")
	//
	// When using pattern hostnames, the instance is resolved dynamically at request
	// time.
	Instance string `json:"instance,required"`
	// Target port on the instance
	Port int64 `json:"port,required"`
	paramObj
}

func (r IngressTargetParam) MarshalJSON() (data []byte, err error) {
	type shadow IngressTargetParam
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *IngressTargetParam) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}

type IngressNewParams struct {
	// Human-readable name (lowercase letters, digits, and dashes only; cannot start or
	// end with a dash)
	Name string `json:"name,required"`
	// Routing rules for this ingress
	Rules []IngressRuleParam `json:"rules,omitzero,required"`
	paramObj
}

func (r IngressNewParams) MarshalJSON() (data []byte, err error) {
	type shadow IngressNewParams
	return param.MarshalObject(r, (*shadow)(&r))
}
func (r *IngressNewParams) UnmarshalJSON(data []byte) error {
	return apijson.UnmarshalRoot(data, r)
}
