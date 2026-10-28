package registrypush

import (
	"context"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/ocicache/testutil"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestPushMacOSImagePreservesManifestAndPlatform(t *testing.T) {
	host, _ := newTestRegistry(t)
	img, err := random.Image(256, 1)
	require.NoError(t, err)
	config, err := img.ConfigFile()
	require.NoError(t, err)
	config.OS, config.Architecture = "darwin", "arm64"
	config.Config.Labels = map[string]string{
		images.MacOSMachineVersionLabel: "1", images.MacOSMachineKindLabel: "macos-image",
		images.MacOSMachineFormatLabel: "raw", images.MacOSMachineDiskLabel: "disk.img",
		images.MacOSMachineAuxLabel: "aux.img", images.MacOSMachinePlatformLabel: "config.json",
	}
	img, err = mutate.ConfigFile(img, config)
	require.NoError(t, err)
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	p := paths.New(t.TempDir())
	digest, err := testutil.WriteImage(p, img)
	require.NoError(t, err)
	target := host + "/releases/macos:canary"
	result, err := PushFromCache(context.Background(), p, digest, target, nil, Options{Insecure: true})
	require.NoError(t, err)
	require.Equal(t, digest, result.Digest)
	ref, err := name.ParseReference(host+"/releases/macos@"+digest, name.Insecure)
	require.NoError(t, err)
	published, err := remote.Image(ref)
	require.NoError(t, err)
	actual, err := published.ConfigFile()
	require.NoError(t, err)
	require.Equal(t, config, actual)
}
