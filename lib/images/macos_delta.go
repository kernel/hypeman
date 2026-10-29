package images

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/v1/types"
)

// MacOSDeltaMediaType marks a machine layer that patches the boot disk unpacked
// from the layers beneath it. Its tar holds, in this order, delta.json, the changed
// chunks of disk.img, and aux.img and config.json in full.
const MacOSDeltaMediaType types.MediaType = "application/vnd.hypeman.machine.disk-delta.v1.tar+gzip"

// MacOSDeltaChunkSize is the granularity at which a delta records changed bytes.
const MacOSDeltaChunkSize = 64 << 10

const (
	macOSDeltaVersion          = 1
	macOSDeltaManifestEntry    = "delta.json"
	macOSDeltaPatchEntry       = "disk.img.patch"
	macOSDeltaDisk             = "disk.img"
	macOSDeltaAux              = "aux.img"
	macOSDeltaConfig           = "config.json"
	maxMacOSDeltaManifestBytes = 1 << 20
	macOSDeltaEntries          = 4
)

// MacOSDelta lists the chunks of a boot disk that differ from the disk beneath it.
// Size is the disk's logical size, which the base disk must match.
type MacOSDelta struct {
	Version   int     `json:"version"`
	ChunkSize int64   `json:"chunk_size"`
	Size      int64   `json:"size"`
	Chunks    []int64 `json:"chunks"`
}

// Validate checks a delta before any chunk is applied: a supported version and
// chunk size, a whole number of chunks, and changed chunks in ascending order.
func (d *MacOSDelta) Validate() error {
	if d.Version != macOSDeltaVersion || d.ChunkSize != MacOSDeltaChunkSize {
		return fmt.Errorf("unsupported machine disk delta version %d with chunk size %d", d.Version, d.ChunkSize)
	}
	if d.Size <= 0 || d.Size%d.ChunkSize != 0 {
		return fmt.Errorf("machine disk delta size %d is not a positive multiple of %d", d.Size, d.ChunkSize)
	}
	count := d.Size / d.ChunkSize
	previous := int64(-1)
	for _, index := range d.Chunks {
		if index <= previous || index >= count {
			return fmt.Errorf("machine disk delta chunk %d is out of order or out of range", index)
		}
		previous = index
	}
	return nil
}

// ComputeMacOSDelta compares a derived boot disk with its base chunk by chunk. Both
// files must be the same size and a whole number of chunks.
func ComputeMacOSDelta(basePath, derivedPath string) (MacOSDelta, error) {
	base, err := os.Open(basePath)
	if err != nil {
		return MacOSDelta{}, err
	}
	defer base.Close()
	derived, err := os.Open(derivedPath)
	if err != nil {
		return MacOSDelta{}, err
	}
	defer derived.Close()
	baseInfo, err := base.Stat()
	if err != nil {
		return MacOSDelta{}, err
	}
	derivedInfo, err := derived.Stat()
	if err != nil {
		return MacOSDelta{}, err
	}
	if baseInfo.Size() != derivedInfo.Size() {
		return MacOSDelta{}, fmt.Errorf("derived disk is %d bytes, base disk is %d", derivedInfo.Size(), baseInfo.Size())
	}
	delta := MacOSDelta{Version: macOSDeltaVersion, ChunkSize: MacOSDeltaChunkSize, Size: derivedInfo.Size(), Chunks: []int64{}}
	if err := delta.Validate(); err != nil {
		return MacOSDelta{}, err
	}
	baseReader := bufio.NewReaderSize(base, 4<<20)
	derivedReader := bufio.NewReaderSize(derived, 4<<20)
	baseChunk := make([]byte, MacOSDeltaChunkSize)
	derivedChunk := make([]byte, MacOSDeltaChunkSize)
	for index := int64(0); index*MacOSDeltaChunkSize < delta.Size; index++ {
		if _, err := io.ReadFull(baseReader, baseChunk); err != nil {
			return MacOSDelta{}, fmt.Errorf("read base disk: %w", err)
		}
		if _, err := io.ReadFull(derivedReader, derivedChunk); err != nil {
			return MacOSDelta{}, fmt.Errorf("read derived disk: %w", err)
		}
		if !bytes.Equal(baseChunk, derivedChunk) {
			delta.Chunks = append(delta.Chunks, index)
		}
	}
	return delta, nil
}

// WriteMacOSDeltaTar writes a delta layer's tar. The changed chunks are read from
// derived, which must be the disk the delta was computed against. aux.img and
// config.json are copied in full from bundle.
func WriteMacOSDeltaTar(w io.Writer, delta MacOSDelta, derived, bundle string) error {
	if err := delta.Validate(); err != nil {
		return err
	}
	manifest, err := json.Marshal(delta)
	if err != nil {
		return err
	}
	disk, err := os.Open(derived)
	if err != nil {
		return err
	}
	defer disk.Close()
	archive := tar.NewWriter(w)
	if err := archive.WriteHeader(&tar.Header{Name: macOSDeltaManifestEntry, Mode: 0600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := archive.Write(manifest); err != nil {
		return err
	}
	patchSize := int64(len(delta.Chunks)) * delta.ChunkSize
	if err := archive.WriteHeader(&tar.Header{Name: macOSDeltaPatchEntry, Mode: 0600, Size: patchSize, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	chunk := make([]byte, delta.ChunkSize)
	for _, index := range delta.Chunks {
		if _, err := disk.ReadAt(chunk, index*delta.ChunkSize); err != nil {
			return fmt.Errorf("read changed chunk %d: %w", index, err)
		}
		if _, err := archive.Write(chunk); err != nil {
			return err
		}
	}
	for _, name := range []string{macOSDeltaAux, macOSDeltaConfig} {
		if err := writeMacOSDeltaFile(archive, filepath.Join(bundle, name), name); err != nil {
			return err
		}
	}
	return archive.Close()
}

func writeMacOSDeltaFile(archive *tar.Writer, path, name string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(archive, file)
	return err
}

// macOSDeltaApplier applies the entries of a delta layer to the boot disk already
// unpacked into targetDir. The entries must arrive in the order WriteMacOSDeltaTar
// writes them, so a layer cannot name files outside the format.
type macOSDeltaApplier struct {
	targetDir string
	step      int
	manifest  *MacOSDelta
}

func (a *macOSDeltaApplier) entry(name string, size int64, archive io.Reader) error {
	switch a.step {
	case 0:
		if name != macOSDeltaManifestEntry {
			return fmt.Errorf("delta layer must start with %s, got %q", macOSDeltaManifestEntry, name)
		}
		if size > maxMacOSDeltaManifestBytes {
			return fmt.Errorf("machine disk delta manifest is %d bytes", size)
		}
		decoder := json.NewDecoder(io.LimitReader(archive, size))
		decoder.DisallowUnknownFields()
		var delta MacOSDelta
		if err := decoder.Decode(&delta); err != nil {
			return fmt.Errorf("decode machine disk delta: %w", err)
		}
		if err := delta.Validate(); err != nil {
			return err
		}
		a.manifest = &delta
	case 1:
		if name != macOSDeltaPatchEntry {
			return fmt.Errorf("delta layer entry %q must be %s", name, macOSDeltaPatchEntry)
		}
		if size != int64(len(a.manifest.Chunks))*a.manifest.ChunkSize {
			return fmt.Errorf("machine disk patch is %d bytes, want %d", size, int64(len(a.manifest.Chunks))*a.manifest.ChunkSize)
		}
		if err := a.applyChunks(archive); err != nil {
			return err
		}
	case 2, 3:
		want := macOSDeltaAux
		if a.step == 3 {
			want = macOSDeltaConfig
		}
		if name != want {
			return fmt.Errorf("delta layer entry %q must be %s", name, want)
		}
		if err := writeSparseFile(filepath.Join(a.targetDir, name), archive, size); err != nil {
			return err
		}
	default:
		return fmt.Errorf("delta layer has more than %d entries", macOSDeltaEntries)
	}
	a.step++
	return nil
}

// applyChunks writes each changed chunk over the base disk in place. The base must
// be a regular file of the delta's size, so the offsets land where the diff computed.
func (a *macOSDeltaApplier) applyChunks(r io.Reader) error {
	path := filepath.Join(a.targetDir, macOSDeltaDisk)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("delta has no base disk: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != a.manifest.Size {
		return fmt.Errorf("base disk does not match the delta's %d-byte size", a.manifest.Size)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	buf := make([]byte, a.manifest.ChunkSize)
	for _, index := range a.manifest.Chunks {
		if _, err = io.ReadFull(r, buf); err != nil {
			break
		}
		if _, err = file.WriteAt(buf, index*a.manifest.ChunkSize); err != nil {
			break
		}
	}
	return errors.Join(err, file.Close())
}

// complete reports an error if the layer ended before its last entry.
func (a *macOSDeltaApplier) complete() error {
	if a.step != macOSDeltaEntries {
		return fmt.Errorf("delta layer has %d of %d entries", a.step, macOSDeltaEntries)
	}
	return nil
}
