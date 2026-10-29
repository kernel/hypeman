package builds

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/images"
)

// maxMachineMetadataBytes bounds a base manifest or config fetched for a delta.
const maxMachineMetadataBytes = 4 << 20

// MachineDeltaBase is a pinned base that a derivative is published against. Its
// layers are mounted from Repo, so the base must already be in the registry the
// publisher writes to.
type MachineDeltaBase struct {
	Repo     string // builds/{id} repository holding the base manifest and layers
	Digest   string // canonical sha256 digest of the base manifest
	DiskPath string // absolute path of the base boot disk on this host
}

func (m MachineDeltaBase) validate() error {
	if !strings.HasPrefix(m.Repo, "builds/") || strings.Contains(m.Repo, "..") || len(m.Repo) > 256 {
		return fmt.Errorf("delta base repository must be under builds/")
	}
	if !ValidMachineDigest(m.Digest) {
		return fmt.Errorf("delta base digest must be a canonical sha256 digest")
	}
	if !filepath.IsAbs(m.DiskPath) {
		return fmt.Errorf("delta base disk path must be absolute")
	}
	return nil
}

// machineLayers returns a machine image's layer blobs and their diff IDs. A
// standalone image has one uploaded layer. A derivative mounts the base's layers
// and uploads one delta layer that patches the base disk.
func machineLayers(ctx context.Context, client *http.Client, token, registry, bundle string, source *MachineDeltaBase) ([]machineBlob, []v1.Hash, error) {
	if source == nil {
		layer, err := machineTarballLayer(ctx, bundle)
		if err != nil {
			return nil, nil, err
		}
		diffID, err := layer.DiffID()
		if err != nil {
			return nil, nil, err
		}
		digest, err := layer.Digest()
		if err != nil {
			return nil, nil, err
		}
		size, err := layer.Size()
		if err != nil {
			return nil, nil, err
		}
		blob := machineBlob{open: layer.Compressed, descriptor: v1.Descriptor{MediaType: types.OCILayer, Digest: digest, Size: size}}
		return []machineBlob{blob}, []v1.Hash{diffID}, nil
	}
	baseLayers, baseDiffIDs, err := fetchMachineSource(ctx, client, token, registry, source)
	if err != nil {
		return nil, nil, err
	}
	blobs := make([]machineBlob, 0, len(baseLayers)+1)
	for _, descriptor := range baseLayers {
		blobs = append(blobs, machineBlob{descriptor: descriptor, mountFrom: source.Repo})
	}
	diskPath := filepath.Join(bundle, "disk.img")
	delta, err := images.ComputeMacOSDelta(source.DiskPath, diskPath)
	if err != nil {
		return nil, nil, fmt.Errorf("compute disk delta: %w", err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		reader, writer := io.Pipe()
		go func() { writer.CloseWithError(images.WriteMacOSDeltaTar(writer, delta, diskPath, bundle)) }()
		return reader, nil
	}, tarball.WithMediaType(images.MacOSDeltaMediaType), tarball.WithCompressionLevel(gzip.DefaultCompression))
	if err != nil {
		return nil, nil, err
	}
	diffID, err := layer.DiffID()
	if err != nil {
		return nil, nil, err
	}
	digest, err := layer.Digest()
	if err != nil {
		return nil, nil, err
	}
	size, err := layer.Size()
	if err != nil {
		return nil, nil, err
	}
	blobs = append(blobs, machineBlob{open: layer.Compressed, descriptor: v1.Descriptor{MediaType: images.MacOSDeltaMediaType, Digest: digest, Size: size}})
	return blobs, append(baseDiffIDs, diffID), nil
}

// fetchMachineSource reads the base manifest and config from the registry, checking
// each against its digest. It returns the base's layer descriptors and diff IDs.
func fetchMachineSource(ctx context.Context, client *http.Client, token, registry string, source *MachineDeltaBase) ([]v1.Descriptor, []v1.Hash, error) {
	root := strings.TrimRight(registry, "/") + "/v2/" + source.Repo
	manifestBytes, err := machineFetch(ctx, client, token, root+"/manifests/"+source.Digest, source.Digest, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("base manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, nil, fmt.Errorf("base manifest: %w", err)
	}
	configDigest := manifest.Config.Digest.String()
	configBytes, err := machineFetch(ctx, client, token, root+"/blobs/"+configDigest, configDigest, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("base config: %w", err)
	}
	var config v1.ConfigFile
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return nil, nil, fmt.Errorf("base config: %w", err)
	}
	if config.OS != "darwin" || config.Architecture != "arm64" || len(manifest.Layers) == 0 || len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return nil, nil, fmt.Errorf("base is not a darwin/arm64 machine image with matching layers")
	}
	return manifest.Layers, config.RootFS.DiffIDs, nil
}

// machineDeltaBase returns the base as a delta source when the publisher writes to
// the registry that stores the base. Any other base publishes standalone.
func (m *manager) machineDeltaBase(publisher MachineBuildPublisher, baseImage string, base *images.Image) *MachineDeltaBase {
	publisherWithRegistry, ok := publisher.(*StreamingMachinePublisher)
	if !ok || base == nil {
		return nil
	}
	address := publisherWithRegistry.RegistryURL
	if !strings.Contains(address, "://") {
		address = "https://" + address
	}
	registry, err := url.Parse(address)
	if err != nil || registry.Host == "" {
		return nil
	}
	ref, err := images.ParseNormalizedRef(baseImage)
	if err != nil || !ref.IsDigest() {
		return nil
	}
	prefix := registry.Host + "/"
	if !strings.HasPrefix(ref.Repository(), prefix) {
		return nil
	}
	disk, err := images.GetDiskPath(m.paths, baseImage, base.Digest)
	if err != nil {
		return nil
	}
	return &MachineDeltaBase{Repo: strings.TrimPrefix(ref.Repository(), prefix), Digest: base.Digest, DiskPath: disk}
}

// publishBuild publishes bundle as a derivative when the base is a registry source,
// and as a standalone image otherwise.
func (r *MachineBuildBackend) publishBuild(ctx context.Context, req MachineBuildRequest, bundle string) (MachinePublication, error) {
	if derived, ok := r.Publisher.(interface {
		PublishDerived(context.Context, string, string, MachineDeltaBase) (MachinePublication, error)
	}); ok && req.deltaBase != nil {
		return derived.PublishDerived(ctx, req.ID, bundle, *req.deltaBase)
	}
	return r.Publisher.Publish(ctx, req.ID, bundle)
}
