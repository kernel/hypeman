package images

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/kernel/hypeman/lib/paths"
	digest "github.com/opencontainers/go-digest"
)

// layersSchemaVersion identifies the on-disk layers.json format. A future
// layer-deduplicated layout can branch on it without guessing.
const layersSchemaVersion = 1

// contentRef identifies shared, content-addressed image data by manifest
// digest. It is validated at construction so storage code never handles a
// raw, unchecked digest string.
type contentRef struct {
	digest digest.Digest
}

// parseContentRef accepts a full digest ("sha256:<hex>") or bare hex and
// validates it before returning the reference.
func parseContentRef(value string) (contentRef, error) {
	if !hasDigestPrefix(value) {
		value = "sha256:" + value
	}
	d, err := digest.Parse(value)
	if err != nil {
		return contentRef{}, fmt.Errorf("parse content digest: %w", err)
	}
	return contentRef{digest: d}, nil
}

func hasDigestPrefix(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == ':' {
			return i > 0
		}
	}
	return false
}

// Hex returns the digest hex without the algorithm prefix, matching how
// content directories are keyed on disk.
func (r contentRef) Hex() string {
	return r.digest.Hex()
}

// Digest returns the full "algorithm:hex" digest.
func (r contentRef) Digest() string {
	return r.digest.String()
}

// layerDescriptor is one ordered layer from an image manifest, as pulled.
// Order is significant: layers must be unpacked in slice order, and diff_id
// aligns positionally with the image config's rootfs.diff_ids.
type layerDescriptor struct {
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`            // compressed layer blob digest
	Size      int64  `json:"size"`              // compressed bytes
	DiffID    string `json:"diff_id,omitempty"` // uncompressed diff digest
}

func (d layerDescriptor) validate() error {
	if _, err := digest.Parse(d.Digest); err != nil {
		return fmt.Errorf("layer digest: %w", err)
	}
	if d.Size < 0 {
		return fmt.Errorf("layer %s: negative size %d", d.Digest, d.Size)
	}
	if d.DiffID != "" {
		if _, err := digest.Parse(d.DiffID); err != nil {
			return fmt.Errorf("layer diff_id: %w", err)
		}
	}
	return nil
}

// configDescriptor identifies the image config blob referenced by the pulled
// manifest. Together with the ordered layer list it is enough to recompose
// the manifest without re-inspecting a registry.
type configDescriptor struct {
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

func (d configDescriptor) validate() error {
	if _, err := digest.Parse(d.Digest); err != nil {
		return fmt.Errorf("config digest: %w", err)
	}
	if d.Size < 0 {
		return fmt.Errorf("config %s: negative size %d", d.Digest, d.Size)
	}
	return nil
}

// imageLayers is the persisted manifest-level record for one image digest:
// the config descriptor plus the ordered layer descriptors. It is metadata
// only: the bootable rootfs disk stays a flattened image and VM boot never
// reads this file. It exists so future work can deduplicate and recompose
// layers across images without re-pulling from a registry.
type imageLayers struct {
	SchemaVersion int               `json:"schema_version"`
	Config        configDescriptor  `json:"config"`
	Layers        []layerDescriptor `json:"layers"`
}

func (l *imageLayers) validate() error {
	if err := l.Config.validate(); err != nil {
		return err
	}
	for i, layer := range l.Layers {
		if err := layer.validate(); err != nil {
			return fmt.Errorf("layer %d: %w", i, err)
		}
	}
	return nil
}

// newImageLayers builds the persisted record from the manifest's config
// descriptor and ordered layer list.
func newImageLayers(config configDescriptor, layers []layerDescriptor) *imageLayers {
	return &imageLayers{
		SchemaVersion: layersSchemaVersion,
		Config:        config,
		Layers:        layers,
	}
}

// writeImageLayers persists the ordered layer descriptors beside the image's
// metadata in the active layout. The write is atomic (temp file + rename),
// matching writeMetadataFile.
func writeImageLayers(p *paths.Paths, repository string, ref contentRef, record *imageLayers) error {
	if err := record.validate(); err != nil {
		return fmt.Errorf("validate layers: %w", err)
	}
	path := layersPath(p, repository, ref.Hex())
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal layers: %w", err)
	}
	if err := installAtomically(path, func(tempPath string) error {
		return os.WriteFile(tempPath, data, 0644)
	}); err != nil {
		return fmt.Errorf("write layers: %w", err)
	}
	return nil
}

// readImageLayers reads the persisted layer descriptors for an image in the
// active layout. Returns ErrNotFound when no layers.json has been written,
// which includes all images built before layer tracking shipped.
func readImageLayers(p *paths.Paths, repository string, ref contentRef) (*imageLayers, error) {
	data, err := os.ReadFile(layersPath(p, repository, ref.Hex()))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read layers: %w", err)
	}
	var record imageLayers
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("unmarshal layers: %w", err)
	}
	return &record, nil
}

// layersPath resolves the layers.json location with the same layout rules as
// metadata and disk.
func layersPath(p *paths.Paths, repository, digestHex string) string {
	return resolveImageLayout(p, repository, digestHex).layers
}
