package paths

import (
	"path/filepath"
	"testing"
)

func TestImageLayerPaths(t *testing.T) {
	p := New("/data")
	repository := "docker.io/library/alpine"
	digestHex := "abababababababababababababababababababababababababababababababab"

	wantContent := filepath.Join("/data", "images", "content", digestHex, "layers.json")
	if got := p.ImageContentLayers(digestHex); got != wantContent {
		t.Errorf("ImageContentLayers = %q, want %q", got, wantContent)
	}

	wantLegacy := filepath.Join("/data", "images", repository, digestHex, "layers.json")
	if got := p.ImageLayers(repository, digestHex); got != wantLegacy {
		t.Errorf("ImageLayers = %q, want %q", got, wantLegacy)
	}
}
