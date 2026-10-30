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
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/images"
)

// StreamingMachinePublisher targets only the configured internal registry and a
// build-scoped repository. go-containerregistry hashes and compresses the layer
// from a reopenable tar stream, so it is never written to disk. Blobs use the
// registry's disk-streaming monolithic upload and are independently verified before
// the manifest is committed last.
// No credentials, redirect destinations or arbitrary output repositories come from the source.
type StreamingMachinePublisher struct {
	RegistryURL string
	Tokens      *RegistryTokenGenerator
}

type machineBlob struct {
	open       func() (io.ReadCloser, error)
	descriptor v1.Descriptor
}

// Publish commits bundle as a standalone machine image in builds/{id}.
func (p *StreamingMachinePublisher) Publish(ctx context.Context, id, bundle string) (MachinePublication, error) {
	return p.publish(ctx, id, bundle, nil)
}

// PublishDerived commits bundle as a delta over base. The base layers are mounted
// from base.Repo rather than uploaded; only the delta layer and config are uploaded.
func (p *StreamingMachinePublisher) PublishDerived(ctx context.Context, id, bundle string, base MachineDeltaBase) (MachinePublication, error) {
	if err := base.validate(); err != nil {
		return MachinePublication{}, err
	}
	return p.publish(ctx, id, bundle, &base)
}

func (p *StreamingMachinePublisher) publish(ctx context.Context, id, bundle string, source *MachineDeltaBase) (MachinePublication, error) {
	deadline, ok := ctx.Deadline()
	if !ok || ctx.Err() != nil {
		return MachinePublication{}, fmt.Errorf("bounded publication required")
	}
	if id == "" || len(id) > 128 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || p.Tokens == nil {
		return MachinePublication{}, fmt.Errorf("scoped machine publisher required")
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
	if err := validateMachineExport(bundle, nil); err != nil {
		return MachinePublication{}, err
	}
	repo := "builds/" + id
	access := []RepoPermission{{Repo: repo, Scope: "push"}}
	if source != nil {
		access = append(access, RepoPermission{Repo: source.Repo, Scope: "pull"})
	}
	token, err := p.Tokens.GenerateToken(id, access, time.Until(deadline))
	if err != nil {
		return MachinePublication{}, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := strings.TrimRight(address, "/") + "/v2/" + repo
	layers, diffIDs, err := machineLayers(ctx, client, token, address, bundle, source)
	if err != nil {
		return MachinePublication{}, err
	}
	config := v1.ConfigFile{OS: "darwin", Architecture: "arm64", RootFS: v1.RootFS{Type: "layers", DiffIDs: diffIDs}, Config: v1.Config{Labels: map[string]string{
		images.MacOSMachineVersionLabel: "1", images.MacOSMachineKindLabel: "macos-image", images.MacOSMachineFormatLabel: "raw", images.MacOSMachineDiskLabel: "disk.img", images.MacOSMachineAuxLabel: "aux.img", images.MacOSMachinePlatformLabel: "config.json",
	}}}
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
		stored, e := machineBlobStored(ctx, client, token, base, blob.descriptor)
		if e != nil {
			return MachinePublication{}, e
		}
		if stored {
			continue
		}
		body, e := blob.open()
		if e != nil {
			return MachinePublication{}, e
		}
		response, e := machineRegistryRequest(ctx, client, token, http.MethodPost, base+"/blobs/uploads/?digest="+blob.descriptor.Digest.String(), "application/octet-stream", io.LimitReader(body, blob.descriptor.Size), blob.descriptor.Size)
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
		if _, e = machineFetch(ctx, client, token, base+"/blobs/"+blob.descriptor.Digest.String(), blob.descriptor.Digest.String(), blob.descriptor.Size); e != nil {
			return MachinePublication{}, fmt.Errorf("machine blob verification failed: %w", e)
		}
	}
	target := base + "/manifests/latest"
	response, err := machineRegistryRequest(ctx, client, token, http.MethodPut, target, string(types.OCIManifestSchema1), bytes.NewReader(manifestBytes), int64(len(manifestBytes)))
	if err != nil {
		return MachinePublication{}, err
	}
	status := response.StatusCode
	response.Body.Close()
	if status != http.StatusCreated {
		return MachinePublication{}, fmt.Errorf("machine manifest commit unconfirmed")
	}
	// The digest covers manifestBytes exactly, so a matching fetch is the same bytes.
	if _, err = machineFetch(ctx, client, token, base+"/manifests/"+digest, digest, int64(len(manifestBytes))); err != nil {
		return MachinePublication{}, fmt.Errorf("machine manifest verification failed: %w", err)
	}
	return MachinePublication{Reference: registry.Host + "/" + repo + "@" + digest, Digest: digest}, nil
}

func machineTarballLayer(ctx context.Context, bundle string) (v1.Layer, error) {
	return tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		reader, writer := io.Pipe()
		go func() { writer.CloseWithError(writeMachineTar(ctx, bundle, writer)) }()
		return reader, nil
	}, tarball.WithMediaType(types.OCILayer), tarball.WithCompressionLevel(gzip.DefaultCompression))
}

// writeMachineTar writes a deterministic tar of the exported bundle.
func writeMachineTar(ctx context.Context, bundle string, w io.Writer) error {
	archive := tar.NewWriter(w)
	for _, name := range []string{"disk.img", "aux.img", "config.json"} {
		source, e := os.Open(filepath.Join(bundle, name))
		if e != nil {
			return e
		}
		info, e := source.Stat()
		if e != nil {
			source.Close()
			return e
		}
		e = archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg})
		if e == nil {
			_, e = io.CopyN(archive, buildContextReader{ctx, source}, info.Size())
		}
		closeErr := source.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return archive.Close()
}
