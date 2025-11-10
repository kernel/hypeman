package images

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onkernel/hypeman/lib/oapi"
)

type imageMetadata struct {
	Name       string                   `json:"name"`
	Status     string                   `json:"status"`
	Error      *string                  `json:"error,omitempty"`
	Request    *oapi.CreateImageRequest `json:"request,omitempty"`
	SizeBytes  int64                    `json:"size_bytes"`
	Entrypoint []string                 `json:"entrypoint,omitempty"`
	Cmd        []string                 `json:"cmd,omitempty"`
	Env        map[string]string        `json:"env,omitempty"`
	WorkingDir string                   `json:"working_dir,omitempty"`
	CreatedAt  time.Time                `json:"created_at"`
}

func (m *imageMetadata) toOAPI() *oapi.Image {
	img := &oapi.Image{
		Name:      m.Name,
		Status:    oapi.ImageStatus(m.Status),
		Error:     m.Error,
		CreatedAt: m.CreatedAt,
	}

	if m.Status == StatusReady && m.SizeBytes > 0 {
		sizeBytes := m.SizeBytes
		img.SizeBytes = &sizeBytes
	}

	if len(m.Entrypoint) > 0 {
		img.Entrypoint = &m.Entrypoint
	}
	if len(m.Cmd) > 0 {
		img.Cmd = &m.Cmd
	}
	if len(m.Env) > 0 {
		img.Env = &m.Env
	}
	if m.WorkingDir != "" {
		img.WorkingDir = &m.WorkingDir
	}

	return img
}

// imageNameToPath converts image name to nested directory structure
// docker.io/library/alpine:latest → docker.io/library/alpine/latest
func imageNameToPath(name string) string {
	// Split on last colon to separate tag
	lastColon := strings.LastIndex(name, ":")
	if lastColon == -1 {
		// No tag, use "latest"
		return filepath.Join(name, "latest")
	}
	
	imagePath := name[:lastColon]
	tag := name[lastColon+1:]
	return filepath.Join(imagePath, tag)
}

func imageDir(dataDir, imageName string) string {
	return filepath.Join(dataDir, "images", imageNameToPath(imageName))
}

func imagePath(dataDir, imageName string) string {
	return filepath.Join(imageDir(dataDir, imageName), "rootfs.erofs")
}

func metadataPath(dataDir, imageName string) string {
	return filepath.Join(imageDir(dataDir, imageName), "metadata.json")
}

func writeMetadata(dataDir, imageName string, meta *imageMetadata) error {
	dir := imageDir(dataDir, imageName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create image directory: %w", err)
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	tempPath := metadataPath(dataDir, imageName) + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("write temp metadata: %w", err)
	}

	finalPath := metadataPath(dataDir, imageName)
	if err := os.Rename(tempPath, finalPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("rename metadata: %w", err)
	}

	return nil
}

func readMetadata(dataDir, imageName string) (*imageMetadata, error) {
	path := metadataPath(dataDir, imageName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var meta imageMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	if meta.Status == StatusReady {
		diskPath := imagePath(dataDir, imageName)
		if _, err := os.Stat(diskPath); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("disk image missing: %s", diskPath)
			}
			return nil, fmt.Errorf("stat disk image: %w", err)
		}
	}

	return &meta, nil
}

func listMetadata(dataDir string) ([]*imageMetadata, error) {
	imagesDir := filepath.Join(dataDir, "images")
	var metas []*imageMetadata

	err := filepath.Walk(imagesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		
		if info.Name() == "metadata.json" {
			// Read metadata file
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			
			var meta imageMetadata
			if err := json.Unmarshal(data, &meta); err != nil {
				return nil
			}
			
			metas = append(metas, &meta)
		}
		
		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("walk images directory: %w", err)
	}

	return metas, nil
}

func imageExists(dataDir, imageName string) bool {
	_, err := readMetadata(dataDir, imageName)
	return err == nil
}

func deleteImage(dataDir, imageName string) error {
	dir := imageDir(dataDir, imageName)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("stat image directory: %w", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove image directory: %w", err)
	}

	return nil
}
