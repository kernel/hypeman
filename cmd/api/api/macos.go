package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/oapi"
)

// macOSHost is true where macOS guests can exist. Tests override it.
var macOSHost = runtime.GOOS == "darwin"

// isMacOSImage reports whether a create names an imported macOS image. Other
// hosts skip the lookup, so Linux creates are unchanged. An image that is not
// cached yet is not a macOS image; any other lookup error is returned.
func (s *ApiService) isMacOSImage(ctx context.Context, name string) (bool, error) {
	if !macOSHost || s.ImageManager == nil {
		return false, nil
	}
	img, err := s.ImageManager.GetImage(ctx, name)
	if errors.Is(err, images.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return img.MacOS != nil, nil
}

// macOSIngressTargetReason refuses routes to macOS guests. Their addresses are NAT
// leases, and no ingress path has been proven for them, so mixed-mode servers must
// not route public traffic to them. A target that cannot be looked up is refused.
func (s *ApiService) macOSIngressTargetReason(ctx context.Context, rules []oapi.IngressRule) string {
	if !macOSHost || s.InstanceManager == nil {
		return ""
	}
	for _, rule := range rules {
		inst, err := s.InstanceManager.GetInstance(ctx, rule.Target.Instance)
		if err != nil {
			return "cannot verify ingress target is not a macOS guest"
		}
		if inst.MacOS != nil {
			return "ingress cannot target macOS guests"
		}
	}
	return ""
}

// Linux defaults for CreateInstanceRequest in openapi.yaml.
const (
	linuxDefaultSize        = "1GB"
	linuxDefaultOverlaySize = "10GB"
	linuxDefaultVcpus       = 2
)

type createFieldsKey struct{}

const maxRecordedCreateBody = 1 << 20

// RecordCreateFields notes which shaping fields a create body sets before the
// request validator fills in the Linux defaults. Only macOS hosts install it.
func RecordCreateFields(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/instances" || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		// This runs before authentication, so only a bounded prefix is buffered.
		// A larger body passes through unrecorded.
		data, err := io.ReadAll(io.LimitReader(r.Body, maxRecordedCreateBody+1))
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(data), r.Body), r.Body}
		var fields map[string]json.RawMessage
		if err != nil || len(data) > maxRecordedCreateBody || json.Unmarshal(data, &fields) != nil {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), createFieldsKey{}, fields)))
	})
}

// ignoreLinuxCreateDefaults clears create fields that hold their Linux schema
// default and that the client did not send. The request validator writes those
// defaults into every body; macOS defaults come from the image. Without a record
// of the sent fields, any value equal to a Linux default is treated as omitted.
func ignoreLinuxCreateDefaults(ctx context.Context, body *oapi.CreateInstanceJSONRequestBody) {
	fields, recorded := ctx.Value(createFieldsKey{}).(map[string]json.RawMessage)
	injected := func(name string) bool {
		_, sent := fields[name]
		return !recorded || !sent
	}
	if body.Size != nil && *body.Size == linuxDefaultSize && injected("size") {
		body.Size = nil
	}
	if body.OverlaySize != nil && *body.OverlaySize == linuxDefaultOverlaySize && injected("overlay_size") {
		body.OverlaySize = nil
	}
	if body.Vcpus != nil && *body.Vcpus == linuxDefaultVcpus && injected("vcpus") {
		body.Vcpus = nil
	}
}
