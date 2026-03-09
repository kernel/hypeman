package snapshottransfer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

type extentRef struct {
	path       string
	fileOffset int64
	length     int64
	dataOffset int64
}

// BuildManifest constructs a deterministic sparse-aware manifest and chunk table.
func BuildManifest(guestDir string, chunkSize int64) (Manifest, []extentRef, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	entries, extents, dataSize, err := scanGuestEntries(guestDir)
	if err != nil {
		return Manifest{}, nil, err
	}

	chunks, err := buildChunkDescriptors(guestDir, extents, dataSize, chunkSize)
	if err != nil {
		return Manifest{}, nil, err
	}

	return Manifest{
		Version:   1,
		ChunkSize: chunkSize,
		DataSize:  dataSize,
		Entries:   entries,
		Chunks:    chunks,
	}, extents, nil
}

func scanGuestEntries(guestDir string) ([]ManifestEntry, []extentRef, int64, error) {
	paths := make([]string, 0, 128)
	if err := filepath.WalkDir(guestDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == guestDir {
			return nil
		}
		rel, err := filepath.Rel(guestDir, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	}); err != nil {
		return nil, nil, 0, fmt.Errorf("walk guest dir: %w", err)
	}
	sort.Strings(paths)

	entries := make([]ManifestEntry, 0, len(paths))
	extents := make([]extentRef, 0, len(paths))
	var dataOffset int64
	for _, rel := range paths {
		full := filepath.Join(guestDir, rel)
		info, err := os.Lstat(full)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("stat %s: %w", rel, err)
		}

		mode := info.Mode()
		switch {
		case mode.IsDir():
			entries = append(entries, ManifestEntry{Path: rel, Type: EntryTypeDirectory, Mode: uint32(mode.Perm())})
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("readlink %s: %w", rel, err)
			}
			entries = append(entries, ManifestEntry{Path: rel, Type: EntryTypeSymlink, Mode: uint32(mode.Perm()), LinkTarget: target})
		case mode.IsRegular():
			fileExtents, err := listFileDataExtents(full)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("list sparse extents for %s: %w", rel, err)
			}
			mext := make([]DataExtent, 0, len(fileExtents))
			for _, ex := range fileExtents {
				e := DataExtent{FileOffset: ex.FileOffset, Length: ex.Length, DataOffset: dataOffset}
				mext = append(mext, e)
				extents = append(extents, extentRef{path: rel, fileOffset: ex.FileOffset, length: ex.Length, dataOffset: dataOffset})
				dataOffset += ex.Length
			}
			entries = append(entries, ManifestEntry{Path: rel, Type: EntryTypeFile, Mode: uint32(mode.Perm()), Size: info.Size(), Extents: mext})
		default:
			return nil, nil, 0, fmt.Errorf("unsupported file type for %s", rel)
		}
	}

	return entries, extents, dataOffset, nil
}

func buildChunkDescriptors(guestDir string, extents []extentRef, dataSize, chunkSize int64) ([]ChunkDescriptor, error) {
	if dataSize == 0 {
		return nil, nil
	}
	chunks := make([]ChunkDescriptor, 0, int((dataSize+chunkSize-1)/chunkSize))
	for offset, idx := int64(0), 0; offset < dataSize; idx++ {
		size := chunkSize
		if remaining := dataSize - offset; remaining < size {
			size = remaining
		}
		h := sha256.New()
		if err := readDataRange(guestDir, extents, offset, size, h); err != nil {
			return nil, err
		}
		chunks = append(chunks, ChunkDescriptor{Index: idx, Offset: offset, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))})
		offset += size
	}
	return chunks, nil
}

func readDataRange(guestDir string, extents []extentRef, offset, length int64, w io.Writer) error {
	if length == 0 {
		return nil
	}
	end := offset + length
	remaining := length
	for _, ex := range extents {
		exStart := ex.dataOffset
		exEnd := ex.dataOffset + ex.length
		if exEnd <= offset || exStart >= end {
			continue
		}

		segStart := max64(offset, exStart)
		segEnd := min64(end, exEnd)
		segLen := segEnd - segStart
		if segLen <= 0 {
			continue
		}

		filePath := filepath.Join(guestDir, ex.path)
		f, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("open %s: %w", ex.path, err)
		}
		fileOffset := ex.fileOffset + (segStart - exStart)
		if _, err := f.Seek(fileOffset, io.SeekStart); err != nil {
			f.Close()
			return fmt.Errorf("seek %s: %w", ex.path, err)
		}
		if _, err := io.CopyN(w, f, segLen); err != nil {
			f.Close()
			return fmt.Errorf("read %s: %w", ex.path, err)
		}
		f.Close()
		remaining -= segLen
		if remaining == 0 {
			break
		}
	}
	if remaining != 0 {
		return fmt.Errorf("data range [%d,%d) not fully covered by manifest extents", offset, end)
	}
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
