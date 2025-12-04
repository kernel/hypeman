package ingress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/onkernel/hypeman/lib/paths"
)

// Filesystem structure:
// {dataDir}/ingresses/{ingress-id}.json

// storedIngress represents ingress data that is persisted to disk.
type storedIngress struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Rules     []IngressRule `json:"rules"`
	CreatedAt string        `json:"created_at"` // RFC3339 format
}

// ensureIngressDir creates the ingresses directory if it doesn't exist.
func ensureIngressDir(p *paths.Paths) error {
	dir := p.IngressesDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create ingresses directory %s: %w", dir, err)
	}
	return nil
}

// loadIngress loads ingress metadata from disk.
func loadIngress(p *paths.Paths, id string) (*storedIngress, error) {
	metaPath := p.IngressMetadata(id)

	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var stored storedIngress
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	return &stored, nil
}

// saveIngress saves ingress metadata to disk.
func saveIngress(p *paths.Paths, stored *storedIngress) error {
	if err := ensureIngressDir(p); err != nil {
		return err
	}

	metaPath := p.IngressMetadata(stored.ID)

	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	return nil
}

// deleteIngressData removes ingress data from disk.
func deleteIngressData(p *paths.Paths, id string) error {
	metaPath := p.IngressMetadata(id)

	if err := os.Remove(metaPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("remove ingress file: %w", err)
	}

	return nil
}

// listIngressIDs returns all ingress IDs by scanning the ingresses directory.
func listIngressIDs(p *paths.Paths) ([]string, error) {
	ingressesDir := p.IngressesDir()

	// Ensure ingresses directory exists
	if err := os.MkdirAll(ingressesDir, 0755); err != nil {
		return nil, fmt.Errorf("create ingresses directory: %w", err)
	}

	entries, err := os.ReadDir(ingressesDir)
	if err != nil {
		return nil, fmt.Errorf("read ingresses directory: %w", err)
	}

	var ids []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}

		// Extract ID from filename (remove .json suffix)
		id := strings.TrimSuffix(name, ".json")
		ids = append(ids, id)
	}

	return ids, nil
}

// loadAllIngresses loads all ingresses from disk.
func loadAllIngresses(p *paths.Paths) ([]storedIngress, error) {
	ids, err := listIngressIDs(p)
	if err != nil {
		return nil, err
	}

	var ingresses []storedIngress
	for _, id := range ids {
		stored, err := loadIngress(p, id)
		if err != nil {
			// Log but skip errors for individual ingresses
			continue
		}
		ingresses = append(ingresses, *stored)
	}

	return ingresses, nil
}

// ingressExists checks if an ingress with the given ID exists.
func ingressExists(p *paths.Paths, id string) bool {
	metaPath := p.IngressMetadata(id)
	_, err := os.Stat(metaPath)
	return err == nil
}

// findIngressByName finds an ingress by name and returns its stored data.
func findIngressByName(p *paths.Paths, name string) (*storedIngress, error) {
	ingresses, err := loadAllIngresses(p)
	if err != nil {
		return nil, err
	}

	for _, ingress := range ingresses {
		if ingress.Name == name {
			return &ingress, nil
		}
	}

	return nil, ErrNotFound
}

// ingressNameExists checks if an ingress with the given name already exists.
func ingressNameExists(p *paths.Paths, name string) (bool, error) {
	ingressesDir := p.IngressesDir()

	entries, err := os.ReadDir(ingressesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read ingresses directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}

		// Read and check name
		filePath := filepath.Join(ingressesDir, name)
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var stored storedIngress
		if err := json.Unmarshal(data, &stored); err != nil {
			continue
		}

		if stored.Name == name {
			return true, nil
		}
	}

	return false, nil
}
