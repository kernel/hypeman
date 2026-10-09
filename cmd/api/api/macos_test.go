package api

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	require.IsType(t, oapi.CreateBuild400JSONResponse{}, build)
	ingress, err := s.CreateIngress(context.Background(), oapi.CreateIngressRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.CreateIngress400JSONResponse{}, ingress)
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

func TestMacOSSchemaDefersTemplateDefaults(t *testing.T) {
	spec, err := oapi.GetSwagger()
	require.NoError(t, err)
	for _, name := range []string{"size", "vcpus", "overlay_size"} {
		require.Nil(t, spec.Components.Schemas["CreateInstanceRequest"].Value.Properties[name].Value.Default, name)
	}
	m := newCaptureCreateManager(nil)
	s := &ApiService{InstanceManager: m, Config: &config.Config{}}
	platform := "darwin/arm64"
	_, err = s.CreateInstance(context.Background(), oapi.CreateInstanceRequestObject{Body: &oapi.CreateInstanceRequest{Name: "macos", Image: "localhost/macos:spike", Platform: &platform}})
	require.NoError(t, err)
	require.NotNil(t, m.lastReq)
	require.Zero(t, m.lastReq.Vcpus)
	require.Zero(t, m.lastReq.Size)
	require.Zero(t, m.lastReq.OverlaySize)
}
