package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestMacOSOnlyRejectsBuildersAndIngress(t *testing.T) {
	s := &ApiService{Config: &config.Config{MacOSOnly: true}}
	build, err := s.CreateBuild(context.Background(), oapi.CreateBuildRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.CreateBuild501JSONResponse{}, build)
	ingress, err := s.CreateIngress(context.Background(), oapi.CreateIngressRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.CreateIngress501JSONResponse{}, ingress)
}

func TestMacOSExecRejectedBeforeWebsocketUpgrade(t *testing.T) {
	s := &ApiService{}
	inst := &instances.Instance{StoredMetadata: instances.StoredMetadata{MacOS: &images.MacOSImage{}}}
	ctx := mw.WithResolvedInstance(context.Background(), "test", inst)
	r := httptest.NewRequest(http.MethodGet, "/instances/test/exec", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s.ExecHandler(w, r)
	require.Equal(t, http.StatusNotImplemented, w.Code)
	require.Contains(t, w.Body.String(), "not implemented")
}

func TestMacOSStatRejectedBeforeGuestDial(t *testing.T) {
	s := &ApiService{}
	inst := &instances.Instance{StoredMetadata: instances.StoredMetadata{MacOS: &images.MacOSImage{}}, State: instances.StateRunning}
	ctx := mw.WithResolvedInstance(context.Background(), "test", inst)
	resp, err := s.StatInstancePath(ctx, oapi.StatInstancePathRequestObject{Id: "test"})
	require.NoError(t, err)
	unsupported, ok := resp.(oapi.StatInstancePath501JSONResponse)
	require.True(t, ok, "macOS stat must be rejected with 501, got %T", resp)
	require.Equal(t, "unsupported", unsupported.Code)
}

func TestLinuxCreateDefaultsMatchSchema(t *testing.T) {
	spec, err := oapi.GetSwagger()
	require.NoError(t, err)
	props := spec.Components.Schemas["CreateInstanceRequest"].Value.Properties
	require.Equal(t, linuxDefaultSize, props["size"].Value.Default)
	require.Equal(t, linuxDefaultOverlaySize, props["overlay_size"].Value.Default)
	require.EqualValues(t, linuxDefaultVcpus, props["vcpus"].Value.Default)
}

func TestMacOSCreateIgnoresInjectedLinuxDefaults(t *testing.T) {
	useMacOSHost(t)
	m := newCaptureCreateManager(nil)
	s := &ApiService{InstanceManager: m, ImageManager: macOSImageManager{}, Config: &config.Config{}}
	create := func(ctx context.Context, size, overlay *string, vcpus *int) {
		t.Helper()
		_, err := s.CreateInstance(ctx, oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{
			Name: "macos", Image: "localhost/macos:spike", Size: size, OverlaySize: overlay, Vcpus: vcpus,
		}})
		require.NoError(t, err)
	}
	// The request validator fills these in when the client omits them.
	size, overlay, vcpus := linuxDefaultSize, linuxDefaultOverlaySize, linuxDefaultVcpus

	t.Run("injected defaults are dropped", func(t *testing.T) {
		create(context.Background(), &size, &overlay, &vcpus)
		require.Zero(t, m.lastReq.Vcpus)
		require.Zero(t, m.lastReq.Size)
		require.Zero(t, m.lastReq.OverlaySize)
	})

	t.Run("other explicit values reach admission", func(t *testing.T) {
		size, vcpus := "8GB", 4
		create(context.Background(), &size, nil, &vcpus)
		require.Equal(t, 4, m.lastReq.Vcpus)
		require.EqualValues(t, 8<<30, m.lastReq.Size)
	})

	t.Run("sent values equal to a default are kept", func(t *testing.T) {
		var ctx context.Context
		body := `{"name":"macos","image":"localhost/macos:spike","vcpus":2}`
		req := httptest.NewRequest(http.MethodPost, "/instances", strings.NewReader(body))
		RecordCreateFields(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			ctx = r.Context()
			sent, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.JSONEq(t, body, string(sent))
		})).ServeHTTP(httptest.NewRecorder(), req)
		require.NotNil(t, ctx)
		create(ctx, &size, &overlay, &vcpus)
		require.Equal(t, 2, m.lastReq.Vcpus)
		require.Zero(t, m.lastReq.Size)
		require.Zero(t, m.lastReq.OverlaySize)
	})
}

func TestRecordCreateFieldsPassesLargeBodiesThrough(t *testing.T) {
	body := `{"name":"macos","pad":"` + strings.Repeat("x", maxRecordedCreateBody) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/instances", strings.NewReader(body))
	RecordCreateFields(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		require.Nil(t, r.Context().Value(createFieldsKey{}))
		sent, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(sent))
	})).ServeHTTP(httptest.NewRecorder(), req)
}

func useMacOSHost(t *testing.T) {
	prev := macOSHost
	macOSHost = true
	t.Cleanup(func() { macOSHost = prev })
}

type macOSImageManager struct{ images.Manager }

func (macOSImageManager) GetImage(context.Context, string) (*images.Image, error) {
	return &images.Image{Platform: "darwin/arm64", MacOS: &images.MacOSImage{}}, nil
}

type macOSIngressTargetManager struct {
	instances.Manager
	inst *instances.Instance
	err  error
}

func (m macOSIngressTargetManager) GetInstance(context.Context, string) (*instances.Instance, error) {
	return m.inst, m.err
}

func TestIngressRejectsMacOSTargets(t *testing.T) {
	useMacOSHost(t)
	rule := oapi.IngressRule{Match: oapi.IngressMatch{Hostname: "app.example.test"}, Target: oapi.IngressTarget{Instance: "mac", Port: 8080}}
	macos := &instances.Instance{StoredMetadata: instances.StoredMetadata{MacOS: &images.MacOSImage{}}}
	s := &ApiService{InstanceManager: macOSIngressTargetManager{inst: macos}}
	require.Contains(t, s.macOSIngressTargetReason(context.Background(), []oapi.IngressRule{rule}), "macOS")

	linux := &instances.Instance{}
	s = &ApiService{InstanceManager: macOSIngressTargetManager{inst: linux}}
	require.Empty(t, s.macOSIngressTargetReason(context.Background(), []oapi.IngressRule{rule}))

	s = &ApiService{InstanceManager: macOSIngressTargetManager{err: errors.New("lookup failed")}}
	require.NotEmpty(t, s.macOSIngressTargetReason(context.Background(), []oapi.IngressRule{rule}), "an unverifiable target is refused")
}
