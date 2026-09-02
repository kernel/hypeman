package images

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernel/hypeman/lib/paths"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// imageManifestModel is the persisted content model for one OCI manifest. It
// records everything needed to recompose the image from shared layer
// artifacts and to decide which OCI blobs are still referenced: the immutable
// manifest digest, the resolved platform, the image config with its diff ids,
// and the ordered layer descriptors.
type imageManifestModel struct {
	SchemaVersion int               `json:"schema_version"`
	Digest        string            `json:"digest"` // manifest digest, sha256:...
	MediaType     string            `json:"media_type,omitempty"`
	Platform      string            `json:"platform"` // os/arch[/variant]
	Config        manifestConfigRef `json:"config"`
	RootFSType    string            `json:"rootfs_type,omitempty"`
	Layers        []layerDescriptor `json:"layers"` // manifest order, base layer first
}

const manifestModelSchemaVersion = 1

type manifestConfigRef struct {
	Digest    string   `json:"digest"` // config blob digest, sha256:...
	MediaType string   `json:"media_type,omitempty"`
	DiffIDs   []string `json:"diff_ids,omitempty"` // uncompressed layer ids, matching Layers order
}

// layerDescriptor describes one compressed layer blob exactly as it appears in
// the manifest, so the blob can be located in the shared OCI cache by digest.
type layerDescriptor struct {
	Digest    string `json:"digest"` // compressed blob digest, sha256:...
	Size      int64  `json:"size"`   // compressed bytes
	MediaType string `json:"media_type,omitempty"`
	DiffID    string `json:"diff_id,omitempty"` // uncompressed diff id from the image config
}

// digestFromHex returns the full sha256 digest string for a bare hex value.
func digestFromHex(hex string) string {
	if strings.HasPrefix(hex, "sha256:") {
		return hex
	}
	return "sha256:" + hex
}

// blobReferences returns every OCI blob digest the manifest depends on: the
// config blob plus all layer blobs. GC must keep these while the manifest is
// referenced.
func (m *imageManifestModel) blobReferences() []string {
	refs := make([]string, 0, len(m.Layers)+1)
	if m.Config.Digest != "" {
		refs = append(refs, m.Config.Digest)
	}
	for _, layer := range m.Layers {
		refs = append(refs, layer.Digest)
	}
	return refs
}

func validateManifestModel(digestHex string, model *imageManifestModel) error {
	if model == nil {
		return fmt.Errorf("manifest model is nil")
	}
	if model.SchemaVersion != manifestModelSchemaVersion {
		return fmt.Errorf("unsupported manifest model schema version: %d", model.SchemaVersion)
	}
	if model.Digest != digestFromHex(digestHex) {
		return fmt.Errorf("manifest model digest %q does not match %q", model.Digest, digestFromHex(digestHex))
	}
	if model.RootFSType != "" && model.RootFSType != "layers" {
		return fmt.Errorf("unsupported manifest rootfs type: %q", model.RootFSType)
	}
	if err := validateManifestConfig(model); err != nil {
		return err
	}
	return validateManifestLayers(model)
}

func validateManifestConfig(model *imageManifestModel) error {
	if model.Config.Digest == "" {
		return fmt.Errorf("manifest model config digest is empty")
	}
	if _, err := parseSHA256Digest(model.Config.Digest); err != nil {
		return fmt.Errorf("invalid manifest model config digest: %q", model.Config.Digest)
	}
	if model.Config.MediaType != "" && convertToOCIMediaType(model.Config.MediaType) != v1.MediaTypeImageConfig {
		return fmt.Errorf("invalid manifest model config media type: %q", model.Config.MediaType)
	}
	if len(model.Config.DiffIDs) != len(model.Layers) {
		return fmt.Errorf("manifest model has %d diff ids for %d layers", len(model.Config.DiffIDs), len(model.Layers))
	}
	return nil
}

func validateManifestLayers(model *imageManifestModel) error {
	for i, layer := range model.Layers {
		if _, err := parseSHA256Digest(layer.Digest); err != nil {
			return fmt.Errorf("invalid manifest model layer %d digest: %q", i, layer.Digest)
		}
		diffID, err := parseSHA256Digest(model.Config.DiffIDs[i])
		if err != nil || layer.DiffID != diffID.String() {
			return fmt.Errorf("invalid manifest model layer %d diff id", i)
		}
		if layer.Size < 0 {
			return fmt.Errorf("invalid manifest model layer %d size: %d", i, layer.Size)
		}
	}
	return nil
}

func parseSHA256Digest(value string) (digest.Digest, error) {
	parsed, err := digest.Parse(value)
	if err != nil || parsed.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("not a sha256 digest")
	}
	return parsed, nil
}

func writeManifestModel(p *paths.Paths, digestHex string, model *imageManifestModel) error {
	return writeManifestModelAt(p.ImageContentManifestModel(digestHex), digestHex, model)
}

func writeManifestModelAt(path, digestHex string, model *imageManifestModel) error {
	if err := validateManifestModel(digestHex, model); err != nil {
		return err
	}
	data, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest model: %w", err)
	}
	return writeJSONAtomic(path, data)
}

// readManifestModel loads the manifest model for a digest, if present.
// Missing models return (nil, nil): images converted before the manifest model
// existed only have a flattened rootfs.
func readManifestModel(p *paths.Paths, digestHex string) (*imageManifestModel, error) {
	data, err := os.ReadFile(p.ImageContentManifestModel(digestHex))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read manifest model: %w", err)
	}
	var model imageManifestModel
	if err := json.Unmarshal(data, &model); err != nil {
		return nil, fmt.Errorf("unmarshal manifest model: %w", err)
	}
	if err := validateManifestModel(digestHex, &model); err != nil {
		return nil, err
	}
	return &model, nil
}

// writeJSONAtomic writes data to path via a temp file in the same directory
// followed by a rename, so readers never observe a partial document.
func writeJSONAtomic(path string, data []byte) error {
	if err := installAtomically(path, func(tempPath string) error {
		return os.WriteFile(tempPath, data, 0o644)
	}); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}
