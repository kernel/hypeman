package builds

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const machineRecipeName = "hypeman-macos-build.json"
const maxMachineExtractBytes = 128 << 20

type machineRecipe struct {
	Version int                 `json:"version"`
	Steps   []machineRecipeStep `json:"steps"`
}
type machineRecipeStep struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
}

var machineEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Extract on the host into an exclusive build-owned directory, never with guest
// tar or a caller-supplied destination. Links/devices, duplicates and bombs fail
// before any build source is copied to the VM.
func extractMachineRecipe(ctx context.Context, source, destination string) (*machineRecipe, error) {
	file, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer compressed.Close()
	compressed.Multistream(false)
	if err = os.Mkdir(destination, 0700); err != nil {
		return nil, err
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
			return nil, err
		}
		header, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		if count >= 1024 {
			return nil, fmt.Errorf("too many machine source entries")
		}
		name := path.Clean(header.Name)
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || !filepath.IsLocal(name) || seen[name] {
			return nil, fmt.Errorf("invalid or duplicate machine source path")
		}
		seen[name] = true
		if header.Size < 0 || header.Size > 64<<20 || total+header.Size > maxMachineExtractBytes {
			return nil, fmt.Errorf("machine source expands beyond limit")
		}
		total += header.Size
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return nil, fmt.Errorf("directory has data")
			}
			if err = os.MkdirAll(target, 0700); err != nil {
				return nil, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return nil, err
			}
			output, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|os.FileMode(header.Mode)&0100)
			if e != nil {
				return nil, e
			}
			_, copyErr := io.Copy(output, reader)
			closeErr := output.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
		default:
			return nil, fmt.Errorf("machine source must contain only regular files and directories")
		}
	}
	// Consume bounded trailing tar padding to verify gzip integrity without allowing
	// an unbounded post-tar decompression stream.
	_, err = io.Copy(io.Discard, limited)
	if err != nil {
		return nil, err
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("machine source expands beyond limit")
	}
	metadata, err := os.Open(filepath.Join(destination, machineRecipeName))
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(metadata, 64<<10+1))
	closeErr := metadata.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > 64<<10 {
		return nil, fmt.Errorf("machine recipe too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var recipe machineRecipe
	if err = decoder.Decode(&recipe); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("recipe must contain one JSON object")
	}
	if recipe.Version != 1 || len(recipe.Steps) == 0 || len(recipe.Steps) > 32 {
		return nil, fmt.Errorf("invalid machine recipe version or step count")
	}
	for _, step := range recipe.Steps {
		if len(step.Command) == 0 || len(step.Command) > 128 || !filepath.IsAbs(step.Command[0]) || len(step.Env) > 128 {
			return nil, fmt.Errorf("invalid machine command")
		}
		for _, arg := range step.Command {
			if len(arg) > 8192 || !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
				return nil, fmt.Errorf("invalid machine argument")
			}
		}
		for key, value := range step.Env {
			if !machineEnvName.MatchString(key) || len(value) > 8192 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
				return nil, fmt.Errorf("invalid machine environment")
			}
		}
	}
	complete = true
	return &recipe, nil
}
