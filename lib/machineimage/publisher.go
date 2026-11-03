// Package machineimage publishes macOS machine images to hypeman's registry, from
// stopped Tart VMs or other ASIF disk chains.
package machineimage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/registrytoken"
)

// Storage is a closed macOS boot disk and platform to publish: the read-only ASIF
// layers below its overlay, base first, the overlay, and its auxiliary storage. A
// standalone base disk is an Overlay with no Layers. LayerDigests, when known, are
// the sha256 of each layer file; otherwise the publisher hashes them.
type Storage struct {
	Layers       []string
	LayerDigests []string
	Overlay      string
	Aux          string
	Platform     images.MacOSImage
}

// Publication is a verified publication: a digest-pinned reference.
type Publication struct{ Reference, Digest string }

// validDigest reports whether s is a canonical sha256 digest.
func validDigest(s string) bool {
	if !strings.HasPrefix(s, "sha256:") || len(s) != 71 {
		return false
	}
	return strings.Trim(s[7:], "0123456789abcdef") == ""
}

// validate checks storage before publication: the platform is valid, the files
// are distinct nonempty regular files, and the overlay stacks on its layers as ASIF.
func (s Storage) validate() error {
	if err := s.Platform.Validate(); err != nil {
		return err
	}
	if len(s.LayerDigests) != 0 && len(s.LayerDigests) != len(s.Layers) {
		return fmt.Errorf("machine storage has %d layer digests for %d layers", len(s.LayerDigests), len(s.Layers))
	}
	if err := images.CheckMachineFiles(append([]string{s.Overlay, s.Aux}, s.Layers...)...); err != nil {
		return err
	}
	disk, err := images.OpenMacOSDiskChain(append(append([]string(nil), s.Layers...), s.Overlay))
	if err != nil {
		return fmt.Errorf("machine disk: %w", err)
	}
	return disk.Close()
}

// Publisher targets only the configured internal registry and a build-scoped
// repository. go-containerregistry hashes and compresses each layer from a
// reopenable tar stream, so nothing is written to disk, skips blobs the registry
// already holds, and commits the manifest last. The registry stores each upload on
// disk and commits it only if it matches its digest.
// No credentials, redirect destinations or arbitrary output repositories come from the source.
type Publisher struct {
	RegistryURL string
	Tokens      *registrytoken.Generator
	// NewMachineIdentifier returns a fresh VZ machine identifier. Every published
	// image gets its own identifier and MAC, so it never runs with its source's.
	NewMachineIdentifier func() ([]byte, error)
}

// Base is a pinned base image in the publisher's registry. A machine published
// over it reuses its layers, so only the new overlay is uploaded.
type Base struct {
	Repo   string // builds/{id} repository holding the base manifest and layers
	Digest string // canonical sha256 digest of the base manifest
}

func (b Base) validate() error {
	if !strings.HasPrefix(b.Repo, "builds/") || strings.Contains(b.Repo, "..") || len(b.Repo) > 256 {
		return fmt.Errorf("machine base repository must be under builds/")
	}
	if !validDigest(b.Digest) {
		return fmt.Errorf("machine base digest must be a canonical sha256 digest")
	}
	return nil
}

// Publish commits storage as a machine image in builds/{id}, with a fresh machine
// identifier and MAC. With a base, the storage's layers must be the base's, by the
// disk digests the base records, and the base's registry layers are reused;
// without one, every layer is uploaded.
func (p *Publisher) Publish(ctx context.Context, id string, storage Storage, base *Base) (Publication, error) {
	deadline, ok := ctx.Deadline()
	if !ok || ctx.Err() != nil {
		return Publication{}, fmt.Errorf("bounded publication required")
	}
	if id == "" || len(id) > 128 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || p.Tokens == nil || p.NewMachineIdentifier == nil {
		return Publication{}, fmt.Errorf("scoped machine publisher required")
	}
	if base != nil {
		if err := base.validate(); err != nil {
			return Publication{}, err
		}
	}
	registry, err := parseRegistryURL(p.RegistryURL)
	if err != nil {
		return Publication{}, err
	}
	if err := storage.validate(); err != nil {
		return Publication{}, err
	}
	if storage.Platform.MachineIdentifier, err = p.NewMachineIdentifier(); err != nil {
		return Publication{}, fmt.Errorf("new machine identifier: %w", err)
	}
	if storage.Platform.MAC, err = randomMAC(); err != nil {
		return Publication{}, err
	}
	repo := "builds/" + id
	access := []registrytoken.RepoPermission{{Repo: repo, Scope: "push"}}
	if base != nil {
		access = append(access, registrytoken.RepoPermission{Repo: base.Repo, Scope: "pull"})
	}
	token, err := p.Tokens.GenerateToken(id, access, time.Until(deadline))
	if err != nil {
		return Publication{}, err
	}
	transport := &http.Transport{}
	if defaults, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaults.Clone()
	}
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	options := []remote.Option{remote.WithContext(ctx), remote.WithAuth(&authn.Bearer{Token: token}), remote.WithTransport(refuseRedirects{transport})}
	var nameOptions []name.Option
	if registry.Scheme == "http" {
		nameOptions = append(nameOptions, name.Insecure)
	}
	img, err := machineImage(ctx, registry.Host, storage, base, nameOptions, options)
	if err != nil {
		return Publication{}, err
	}
	target, err := name.NewTag(registry.Host+"/"+repo+":latest", nameOptions...)
	if err != nil {
		return Publication{}, err
	}
	if err = remote.Write(target, img, options...); err != nil {
		return Publication{}, fmt.Errorf("write machine image: %w", err)
	}
	digest, err := img.Digest()
	if err != nil {
		return Publication{}, err
	}
	return Publication{Reference: registry.Host + "/" + repo + "@" + digest.String(), Digest: digest.String()}, nil
}

// randomMAC returns a random locally administered unicast MAC address.
func randomMAC() (string, error) {
	mac := make([]byte, 6)
	if _, err := rand.Read(mac); err != nil {
		return "", err
	}
	mac[0] = mac[0]&^0x01 | 0x02
	return net.HardwareAddr(mac).String(), nil
}

// refuseRedirects fails a redirected registry request rather than follow it, so
// the token never reaches another host.
type refuseRedirects struct{ http.RoundTripper }

func (t refuseRedirects) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.RoundTripper.RoundTrip(req)
	if err == nil && response.StatusCode >= 300 && response.StatusCode < 400 {
		response.Body.Close()
		return nil, fmt.Errorf("registry redirect refused")
	}
	return response, err
}

// ParseBase returns ref, a digest-pinned image in the registry at registryURL, as a
// publication base.
func ParseBase(registryURL, ref string) (*Base, error) {
	registry, err := parseRegistryURL(registryURL)
	if err != nil {
		return nil, err
	}
	parsed, err := images.ParseNormalizedRef(ref)
	if err != nil || !parsed.IsDigest() {
		return nil, fmt.Errorf("machine base must be pinned by digest")
	}
	repo, ok := strings.CutPrefix(parsed.Repository(), registry.Host+"/")
	if !ok {
		return nil, fmt.Errorf("machine base %s is not in the registry %s", ref, registry.Host)
	}
	base := &Base{Repo: repo, Digest: parsed.Digest()}
	return base, base.validate()
}

// parseRegistryURL parses a registry address, defaulting to HTTPS. Plaintext is
// allowed only on loopback.
func parseRegistryURL(address string) (*url.URL, error) {
	if !strings.Contains(address, "://") {
		address = "https://" + address
	}
	registry, err := url.Parse(address)
	if err != nil || registry.Host == "" || registry.User != nil || registry.RawQuery != "" || registry.Fragment != "" || (registry.Path != "" && registry.Path != "/") || (registry.Scheme != "http" && registry.Scheme != "https") {
		return nil, fmt.Errorf("invalid internal registry address")
	}
	if registry.Scheme == "http" && registry.Hostname() != "localhost" && !net.ParseIP(registry.Hostname()).IsLoopback() {
		return nil, fmt.Errorf("plaintext registry must be loopback")
	}
	return registry, nil
}

// machineImage returns the machine image for storage: the base's layers, from the
// registry or the storage's own layer files, then a top layer with the overlay,
// auxiliary storage and platform. Its labels record every layer's disk digest.
func machineImage(ctx context.Context, host string, storage Storage, base *Base, nameOptions []name.Option, options []remote.Option) (v1.Image, error) {
	layerDigests := storage.LayerDigests
	if len(layerDigests) == 0 {
		for _, path := range storage.Layers {
			digest, err := fileDigest(ctx, path)
			if err != nil {
				return nil, err
			}
			layerDigests = append(layerDigests, digest)
		}
	}
	img := empty.Image
	if base != nil {
		ref, err := name.NewDigest(host+"/"+base.Repo+"@"+base.Digest, nameOptions...)
		if err != nil {
			return nil, err
		}
		if img, err = remote.Image(ref, options...); err != nil {
			return nil, fmt.Errorf("base image: %w", err)
		}
		config, err := img.ConfigFile()
		if err != nil {
			return nil, fmt.Errorf("base config: %w", err)
		}
		if config.OS != "darwin" || config.Architecture != "arm64" {
			return nil, fmt.Errorf("base is not a darwin/arm64 machine image")
		}
		for label, want := range images.MacOSMachineLabels() {
			if config.Config.Labels[label] != want {
				return nil, fmt.Errorf("base is not an ASIF machine image: %s=%q", label, config.Config.Labels[label])
			}
		}
		if layers := len(config.RootFS.DiffIDs); layers != len(storage.Layers) {
			return nil, fmt.Errorf("machine base has %d layers but the disk stacks on %d", layers, len(storage.Layers))
		}
		baseDigests := strings.Split(config.Config.Labels[images.MacOSMachineDiskDigestsLabel], ",")
		if !slices.Equal(baseDigests, layerDigests) {
			return nil, fmt.Errorf("machine base's disks are not the layers the disk stacks on: base records %q", config.Config.Labels[images.MacOSMachineDiskDigestsLabel])
		}
	} else {
		for _, path := range storage.Layers {
			layer, err := machineLayer(ctx, []machineTarEntry{{name: images.MacOSMachineDiskFile, path: path}})
			if err != nil {
				return nil, err
			}
			if img, err = mutate.AppendLayers(img, layer); err != nil {
				return nil, err
			}
		}
	}
	overlayDigest, err := fileDigest(ctx, storage.Overlay)
	if err != nil {
		return nil, err
	}
	platform, err := json.Marshal(storage.Platform)
	if err != nil {
		return nil, err
	}
	top, err := machineLayer(ctx, []machineTarEntry{{name: images.MacOSMachineDiskFile, path: storage.Overlay}, {name: images.MacOSMachineAuxFile, path: storage.Aux}, {name: images.MacOSMachinePlatformFile, data: platform}})
	if err != nil {
		return nil, err
	}
	if img, err = mutate.AppendLayers(img, top); err != nil {
		return nil, err
	}
	config, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	labels := images.MacOSMachineLabels()
	labels[images.MacOSMachineDiskDigestsLabel] = strings.Join(append(slices.Clone(layerDigests), overlayDigest), ",")
	config.OS, config.Architecture, config.Config.Labels = "darwin", "arm64", labels
	if img, err = mutate.ConfigFile(img, config); err != nil {
		return nil, err
	}
	return mutate.ConfigMediaType(mutate.MediaType(img, types.OCIManifestSchema1), types.OCIConfigJSON), nil
}

// fileDigest returns the sha256 digest of a file.
func fileDigest(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx, file}); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// machineTarEntry is one file in a machine layer, from a path or in memory.
type machineTarEntry struct {
	name string
	path string
	data []byte
}

// machineLayer returns a gzipped layer of entries, regenerated from the files
// each time it is read.
func machineLayer(ctx context.Context, entries []machineTarEntry) (v1.Layer, error) {
	return tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		reader, writer := io.Pipe()
		go func() { writer.CloseWithError(writeMachineTar(ctx, entries, writer)) }()
		return reader, nil
	}, tarball.WithMediaType(types.OCILayer), tarball.WithCompressionLevel(gzip.DefaultCompression))
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
			_, err = io.CopyN(archive, contextReader{ctx, source}, info.Size())
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

// contextReader stops a long read when ctx is done.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
