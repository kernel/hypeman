package builds

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// No credentials, redirect destinations or arbitrary output repositories come from recipes.
type StreamingMachinePublisher struct {
	RegistryURL          string
	Tokens               *RegistryTokenGenerator
	BeforeManifestCommit func(context.Context, string, string) error
}

type machineBlob struct {
	open       func() (io.ReadCloser, error)
	descriptor v1.Descriptor
}

func (p *StreamingMachinePublisher) Publish(ctx context.Context, id, bundle string) (MachinePublication, error) {
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
	layer, err := machineTarballLayer(ctx, bundle)
	if err != nil {
		return MachinePublication{}, err
	}
	diffID, err := layer.DiffID()
	if err != nil {
		return MachinePublication{}, err
	}
	layerDigest, err := layer.Digest()
	if err != nil {
		return MachinePublication{}, err
	}
	layerSize, err := layer.Size()
	if err != nil {
		return MachinePublication{}, err
	}
	layerBlob := machineBlob{open: layer.Compressed, descriptor: v1.Descriptor{MediaType: types.OCILayer, Digest: layerDigest, Size: layerSize}}
	config := v1.ConfigFile{OS: "darwin", Architecture: "arm64", RootFS: v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{diffID}}, Config: v1.Config{Labels: map[string]string{
		images.MacOSMachineVersionLabel: "1", images.MacOSMachineKindLabel: "macos-image", images.MacOSMachineFormatLabel: "raw", images.MacOSMachineDiskLabel: "disk.img", images.MacOSMachineAuxLabel: "aux.img", images.MacOSMachinePlatformLabel: "config.json",
	}}}
	configBytes, err := json.Marshal(config)
	if err != nil {
		return MachinePublication{}, err
	}
	configBlob := machineBlob{open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(configBytes)), nil }, descriptor: v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: machineHash(configBytes), Size: int64(len(configBytes))}}
	manifest := v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Config: configBlob.descriptor, Layers: []v1.Descriptor{layerBlob.descriptor}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return MachinePublication{}, err
	}
	digest := machineHash(manifestBytes).String()
	repo := "builds/" + id
	token, err := p.Tokens.GeneratePushToken(id, []string{repo}, time.Until(deadline))
	if err != nil {
		return MachinePublication{}, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := strings.TrimRight(address, "/") + "/v2/" + repo
	for _, blob := range []machineBlob{configBlob, layerBlob} {
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
		response, e = machineRegistryRequest(ctx, client, token, http.MethodGet, base+"/blobs/"+blob.descriptor.Digest.String(), "", nil, 0)
		if e != nil {
			return MachinePublication{}, e
		}
		sum := sha256.New()
		n, e := io.Copy(sum, io.LimitReader(response.Body, blob.descriptor.Size+1))
		response.Body.Close()
		if e != nil || response.StatusCode != http.StatusOK || n != blob.descriptor.Size || hex.EncodeToString(sum.Sum(nil)) != blob.descriptor.Digest.Hex {
			return MachinePublication{}, fmt.Errorf("machine blob verification failed")
		}
	}
	if p.BeforeManifestCommit != nil {
		if err := p.BeforeManifestCommit(ctx, id, digest); err != nil {
			return MachinePublication{}, err
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
	response, err = machineRegistryRequest(ctx, client, token, http.MethodGet, base+"/manifests/"+digest, "", nil, 0)
	if err != nil {
		return MachinePublication{}, err
	}
	published, err := io.ReadAll(io.LimitReader(response.Body, int64(len(manifestBytes))+1))
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(published, manifestBytes) {
		return MachinePublication{}, fmt.Errorf("machine manifest verification failed")
	}
	return MachinePublication{Reference: registry.Host + "/" + repo + "@" + digest, Digest: digest}, nil
}

func machineRegistryRequest(ctx context.Context, client *http.Client, token, method, target, media string, body io.Reader, size int64) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	request.ContentLength = size
	request.Header.Set("Authorization", "Bearer "+token)
	if media != "" {
		request.Header.Set("Content-Type", media)
	}
	request.Header.Set("Accept", string(types.OCIManifestSchema1))
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("machine registry request failed")
	}
	return response, nil
}

func machineHash(data []byte) v1.Hash {
	sum := sha256.Sum256(data)
	return v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

func machineTarballLayer(ctx context.Context, bundle string) (v1.Layer, error) {
	return tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		reader, writer := io.Pipe()
		go func() { writer.CloseWithError(writeMachineTar(ctx, bundle, writer)) }()
		return reader, nil
	}, tarball.WithMediaType(types.OCILayer))
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
