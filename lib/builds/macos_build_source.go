package builds

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// machineEntrypoint is the script a machine build source must contain at its
// root. It runs as root with /bin/sh in the guest's private build directory.
const machineEntrypoint = "build.sh"
const maxMachineExtractBytes = 128 << 20

// extractMachineSource unpacks a build-owned tar.gz on the host into an exclusive
// directory, never with guest tar or a caller-supplied destination. Links,
// devices, duplicates and decompression bombs fail before anything is copied to
// the VM. The archive must contain the entrypoint as a regular file.
func extractMachineSource(ctx context.Context, source, destination string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	compressed.Multistream(false)
	if err = os.Mkdir(destination, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destination)
		}
	}()
	limited := &io.LimitedReader{R: compressed, N: maxMachineExtractBytes + 1}
	reader := tar.NewReader(limited)
	seen := map[string]bool{}
	var total int64
	for count := 0; ; count++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		header, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if count >= 1024 {
			return fmt.Errorf("too many machine source entries")
		}
		name := path.Clean(header.Name)
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || !filepath.IsLocal(name) || seen[name] {
			return fmt.Errorf("invalid or duplicate machine source path")
		}
		seen[name] = true
		if header.Size < 0 || header.Size > 64<<20 || total+header.Size > maxMachineExtractBytes {
			return fmt.Errorf("machine source expands beyond limit")
		}
		total += header.Size
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return fmt.Errorf("directory has data")
			}
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			output, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|os.FileMode(header.Mode)&0100)
			if e != nil {
				return e
			}
			_, copyErr := io.Copy(output, reader)
			closeErr := output.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("machine source must contain only regular files and directories")
		}
	}
	// Consume bounded trailing tar padding to verify gzip integrity without allowing
	// an unbounded post-tar decompression stream.
	if _, err = io.Copy(io.Discard, limited); err != nil {
		return err
	}
	if limited.N <= 0 {
		return fmt.Errorf("machine source expands beyond limit")
	}
	info, err := os.Lstat(filepath.Join(destination, machineEntrypoint))
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("machine source must contain %s", machineEntrypoint)
	}
	complete = true
	return nil
}
