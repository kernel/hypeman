package builds

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/images"
)

// StreamingMachinePublisher targets only the configured internal registry and a
// build-scoped repository. The layer is generated twice from the exported bundle,
// once to hash it and once to stream it, so it is never written to disk. Blobs are
// uploaded with bounded memory, independently re-read/hash verified, then the
// manifest is committed last.
// No credentials, redirect destinations or arbitrary output repositories come from recipes.
type StreamingMachinePublisher struct {
	RegistryURL string
	Tokens      *RegistryTokenGenerator
}

type machineBlob struct {
	open       func() io.ReadCloser
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
	layer, diffID, err := hashMachineLayer(ctx, bundle)
	if err != nil {
		return MachinePublication{}, err
	}
	config := v1.ConfigFile{OS: "darwin", Architecture: "arm64", RootFS: v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{diffID}}, Config: v1.Config{Labels: map[string]string{
		images.MacOSMachineVersionLabel: "1", images.MacOSMachineKindLabel: "macos-image", images.MacOSMachineFormatLabel: "raw", images.MacOSMachineDiskLabel: "disk.img", images.MacOSMachineAuxLabel: "aux.img", images.MacOSMachinePlatformLabel: "config.json",
	}}}
	configBytes, err := json.Marshal(config)
	if err != nil {
		return MachinePublication{}, err
	}
	configBlob := machineBlob{open: func() io.ReadCloser { return io.NopCloser(bytes.NewReader(configBytes)) }, descriptor: v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: machineHash(configBytes), Size: int64(len(configBytes))}}
	manifest := v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Config: configBlob.descriptor, Layers: []v1.Descriptor{layer.descriptor}}
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
	for _, blob := range []machineBlob{configBlob, layer} {
		body := blob.open()
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

// hashMachineLayer hashes the bundle's gzipped tar layer without storing it. The
// returned blob regenerates the same bytes when opened; the registry rejects them
// if the bundle changed in between.
func hashMachineLayer(ctx context.Context, bundle string) (blob machineBlob, diffID v1.Hash, err error) {
	compressed := sha256.New()
	counter := &machineByteCounter{}
	plain, err := writeMachineLayer(ctx, bundle, io.MultiWriter(compressed, counter))
	if err != nil {
		return blob, diffID, err
	}
	open := func() io.ReadCloser {
		reader, writer := io.Pipe()
		go func() {
			_, err := writeMachineLayer(ctx, bundle, writer)
			writer.CloseWithError(err)
		}()
		return reader
	}
	digest := v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(compressed.Sum(nil))}
	return machineBlob{open: open, descriptor: v1.Descriptor{MediaType: types.OCILayer, Digest: digest, Size: counter.n}}, plain, nil
}

type machineByteCounter struct{ n int64 }

func (c *machineByteCounter) Write(data []byte) (int, error) {
	c.n += int64(len(data))
	return len(data), nil
}

// writeMachineLayer writes the bundle as a gzipped tar and returns the digest of
// the uncompressed tar. Headers carry no timestamps, so the output is deterministic.
func writeMachineLayer(ctx context.Context, bundle string, w io.Writer) (diffID v1.Hash, err error) {
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return diffID, err
	}
	plainHash := sha256.New()
	archive := tar.NewWriter(io.MultiWriter(gz, plainHash))
	for _, name := range []string{"disk.img", "aux.img", "config.json"} {
		source, e := os.Open(filepath.Join(bundle, name))
		if e != nil {
			return diffID, e
		}
		info, e := source.Stat()
		if e != nil {
			source.Close()
			return diffID, e
		}
		e = archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg})
		if e == nil {
			_, e = io.CopyN(archive, buildContextReader{ctx, source}, info.Size())
		}
		closeErr := source.Close()
		if e != nil {
			return diffID, e
		}
		if closeErr != nil {
			return diffID, closeErr
		}
	}
	if err = archive.Close(); err != nil {
		return diffID, err
	}
	if err = gz.Close(); err != nil {
		return diffID, err
	}
	return v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(plainHash.Sum(nil))}, nil
}
