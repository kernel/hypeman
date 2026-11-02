package builds

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/images"
)

// StreamingMachinePublisher targets only the configured internal registry and a
// build-scoped repository. go-containerregistry hashes and compresses each layer
// from a reopenable tar stream, so nothing is written to disk. Blobs use the
// registry's disk-streaming monolithic upload and are independently verified before
// the manifest is committed last.
// No credentials, redirect destinations or arbitrary output repositories come from the source.
type StreamingMachinePublisher struct {
	RegistryURL string
	Tokens      *RegistryTokenGenerator
}

// MachineBase is a pinned base image in the publisher's registry. A machine
// published over it reuses its layers, so only the new overlay is uploaded.
type MachineBase struct {
	Repo   string // builds/{id} repository holding the base manifest and layers
	Digest string // canonical sha256 digest of the base manifest
}

func (b MachineBase) validate() error {
	if !strings.HasPrefix(b.Repo, "builds/") || strings.Contains(b.Repo, "..") || len(b.Repo) > 256 {
		return fmt.Errorf("machine base repository must be under builds/")
	}
	if !ValidMachineDigest(b.Digest) {
		return fmt.Errorf("machine base digest must be a canonical sha256 digest")
	}
	return nil
}

// machineLabels describe a published machine image: one ASIF disk per layer, with
// the auxiliary storage and platform in the top layer.
var machineLabels = map[string]string{
	images.MacOSMachineVersionLabel: "2", images.MacOSMachineKindLabel: "macos-image", images.MacOSMachineFormatLabel: "asif",
	images.MacOSMachineDiskLabel: "disk.asif", images.MacOSMachineAuxLabel: "aux.img", images.MacOSMachinePlatformLabel: "config.json",
}

type machineBlob struct {
	open       func() (io.ReadCloser, error)
	descriptor v1.Descriptor
}

// Publish commits storage as a machine image in builds/{id}. With a base, the
// storage's layers must be the base's and the base's registry layers are reused;
// without one, every layer is uploaded.
func (p *StreamingMachinePublisher) Publish(ctx context.Context, id string, storage MachineStorage, base *MachineBase) (MachinePublication, error) {
	deadline, ok := ctx.Deadline()
	if !ok || ctx.Err() != nil {
		return MachinePublication{}, fmt.Errorf("bounded publication required")
	}
	if id == "" || len(id) > 128 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || p.Tokens == nil {
		return MachinePublication{}, fmt.Errorf("scoped machine publisher required")
	}
	if base != nil {
		if err := base.validate(); err != nil {
			return MachinePublication{}, err
		}
	}
	address := p.RegistryURL
	if !strings.Contains(address, "://") {
		address = "https://" + address
	}
	registry, err := url.Parse(address)
	if err != nil || registry.Host == "" || registry.User != nil || registry.RawQuery != "" || registry.Fragment != "" || (registry.Path != "" && registry.Path != "/") || (registry.Scheme != "http" && registry.Scheme != "https") {
		return MachinePublication{}, fmt.Errorf("invalid internal registry address")
	}
	if registry.Scheme == "http" && registry.Hostname() != "localhost" && !net.ParseIP(registry.Hostname()).IsLoopback() {
		return MachinePublication{}, fmt.Errorf("plaintext registry must be loopback")
	}
	if err := validateMachineStorage(storage, nil); err != nil {
		return MachinePublication{}, err
	}
	repo := "builds/" + id
	access := []RepoPermission{{Repo: repo, Scope: "push"}}
	if base != nil {
		access = append(access, RepoPermission{Repo: base.Repo, Scope: "pull"})
	}
	token, err := p.Tokens.GenerateToken(id, access, time.Until(deadline))
	if err != nil {
		return MachinePublication{}, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	target := strings.TrimRight(address, "/") + "/v2/" + repo
	layers, diffIDs, err := machineLayers(ctx, client, token, address, storage, base)
	if err != nil {
		return MachinePublication{}, err
	}
	config := v1.ConfigFile{OS: "darwin", Architecture: "arm64", RootFS: v1.RootFS{Type: "layers", DiffIDs: diffIDs}, Config: v1.Config{Labels: machineLabels}}
	configBytes, err := json.Marshal(config)
	if err != nil {
		return MachinePublication{}, err
	}
	configBlob := machineBlob{open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(configBytes)), nil }, descriptor: v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: machineHash(configBytes), Size: int64(len(configBytes))}}
	descriptors := make([]v1.Descriptor, 0, len(layers))
	for _, layer := range layers {
		descriptors = append(descriptors, layer.descriptor)
	}
	manifest := v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Config: configBlob.descriptor, Layers: descriptors}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return MachinePublication{}, err
	}
	digest := machineHash(manifestBytes).String()
	for _, blob := range append([]machineBlob{configBlob}, layers...) {
		// The blob store is keyed by digest, so a blob the registry already holds
		// (a base layer, or a retried publish) is not uploaded again.
		stored, e := machineBlobStored(ctx, client, token, target, blob.descriptor)
		if e != nil {
			return MachinePublication{}, e
		}
		if stored {
			continue
		}
		if blob.open == nil {
			return MachinePublication{}, fmt.Errorf("base layer %s is missing from the registry", blob.descriptor.Digest)
		}
		body, e := blob.open()
		if e != nil {
			return MachinePublication{}, e
		}
		response, e := machineRegistryRequest(ctx, client, token, http.MethodPost, target+"/blobs/uploads/?digest="+blob.descriptor.Digest.String(), "application/octet-stream", io.LimitReader(body, blob.descriptor.Size), blob.descriptor.Size)
		closeErr := body.Close()
		if e != nil {
			return MachinePublication{}, e
		}
		if closeErr != nil {
			response.Body.Close()
			return MachinePublication{}, closeErr
		}
		status := response.StatusCode
		response.Body.Close()
		if status != http.StatusCreated {
			return MachinePublication{}, fmt.Errorf("machine blob upload was not committed")
		}
		if _, e = machineFetch(ctx, client, token, target+"/blobs/"+blob.descriptor.Digest.String(), blob.descriptor.Digest.String(), blob.descriptor.Size); e != nil {
			return MachinePublication{}, fmt.Errorf("machine blob verification failed: %w", e)
		}
	}
	response, err := machineRegistryRequest(ctx, client, token, http.MethodPut, target+"/manifests/latest", string(types.OCIManifestSchema1), bytes.NewReader(manifestBytes), int64(len(manifestBytes)))
	if err != nil {
		return MachinePublication{}, err
	}
	status := response.StatusCode
	response.Body.Close()
	if status != http.StatusCreated {
		return MachinePublication{}, fmt.Errorf("machine manifest commit unconfirmed")
	}
	// The digest covers manifestBytes exactly, so a matching fetch is the same bytes.
	if _, err = machineFetch(ctx, client, token, target+"/manifests/"+digest, digest, int64(len(manifestBytes))); err != nil {
		return MachinePublication{}, fmt.Errorf("machine manifest verification failed: %w", err)
	}
	return MachinePublication{Reference: registry.Host + "/" + repo + "@" + digest, Digest: digest}, nil
}

// machineLayers returns a machine image's layer blobs and diff IDs: the base's
// layers, from the registry or the storage's own layer files, then a top layer with
// the overlay, auxiliary storage and platform.
func machineLayers(ctx context.Context, client *http.Client, token, registry string, storage MachineStorage, base *MachineBase) ([]machineBlob, []v1.Hash, error) {
	var blobs []machineBlob
	var diffIDs []v1.Hash
	if base != nil {
		descriptors, ids, err := fetchMachineBase(ctx, client, token, registry, *base)
		if err != nil {
			return nil, nil, err
		}
		if len(descriptors) != len(storage.Layers) {
			return nil, nil, fmt.Errorf("machine base has %d layers but the disk stacks on %d", len(descriptors), len(storage.Layers))
		}
		for _, descriptor := range descriptors {
			blobs = append(blobs, machineBlob{descriptor: descriptor})
		}
		diffIDs = ids
	} else {
		for _, layer := range storage.Layers {
			blob, diffID, err := machineLayer(ctx, []machineTarEntry{{name: "disk.asif", path: layer}})
			if err != nil {
				return nil, nil, err
			}
			blobs, diffIDs = append(blobs, blob), append(diffIDs, diffID)
		}
	}
	platform, err := json.Marshal(storage.Platform)
	if err != nil {
		return nil, nil, err
	}
	top, diffID, err := machineLayer(ctx, []machineTarEntry{{name: "disk.asif", path: storage.Overlay}, {name: "aux.img", path: storage.Aux}, {name: "config.json", data: platform}})
	if err != nil {
		return nil, nil, err
	}
	return append(blobs, top), append(diffIDs, diffID), nil
}

// fetchMachineBase reads the base manifest and config from the registry, checking
// each against its digest and that the base is an ASIF machine image. It returns
// the base's layer descriptors and diff IDs.
func fetchMachineBase(ctx context.Context, client *http.Client, token, registry string, base MachineBase) ([]v1.Descriptor, []v1.Hash, error) {
	root := strings.TrimRight(registry, "/") + "/v2/" + base.Repo
	manifestBytes, err := machineFetch(ctx, client, token, root+"/manifests/"+base.Digest, base.Digest, 0)
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
	for label, want := range machineLabels {
		if config.Config.Labels[label] != want {
			return nil, nil, fmt.Errorf("base is not an ASIF machine image: %s=%q", label, config.Config.Labels[label])
		}
	}
	return manifest.Layers, config.RootFS.DiffIDs, nil
}

// machineTarEntry is one file in a machine layer, from a path or in memory.
type machineTarEntry struct {
	name string
	path string
	data []byte
}

// machineLayer returns a gzipped layer of entries, with its descriptor and diff ID.
func machineLayer(ctx context.Context, entries []machineTarEntry) (machineBlob, v1.Hash, error) {
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		reader, writer := io.Pipe()
		go func() { writer.CloseWithError(writeMachineTar(ctx, entries, writer)) }()
		return reader, nil
	}, tarball.WithMediaType(types.OCILayer), tarball.WithCompressionLevel(gzip.DefaultCompression))
	if err != nil {
		return machineBlob{}, v1.Hash{}, err
	}
	diffID, err := layer.DiffID()
	if err != nil {
		return machineBlob{}, v1.Hash{}, err
	}
	digest, err := layer.Digest()
	if err != nil {
		return machineBlob{}, v1.Hash{}, err
	}
	size, err := layer.Size()
	if err != nil {
		return machineBlob{}, v1.Hash{}, err
	}
	return machineBlob{open: layer.Compressed, descriptor: v1.Descriptor{MediaType: types.OCILayer, Digest: digest, Size: size}}, diffID, nil
}

// writeMachineTar writes a deterministic tar of entries: fixed modes and no
// timestamps, so regenerating it yields the bytes that were hashed.
func writeMachineTar(ctx context.Context, entries []machineTarEntry, w io.Writer) error {
	archive := tar.NewWriter(w)
	for _, entry := range entries {
		if entry.path == "" {
			if err := archive.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.data)), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			if _, err := archive.Write(entry.data); err != nil {
				return err
			}
			continue
		}
		source, err := os.Open(entry.path)
		if err != nil {
			return err
		}
		info, err := source.Stat()
		if err == nil {
			err = archive.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg})
		}
		if err == nil {
			_, err = io.CopyN(archive, buildContextReader{ctx, source}, info.Size())
		}
		closeErr := source.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return archive.Close()
}
