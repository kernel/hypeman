package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const maxBuildSourceBytes = 64 << 20

type buildContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r buildContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// stageBuildSource streams compressed source once into a private, exclusive job
// file. Both backends consume this file and use its hash for input verification.
func stageBuildSource(ctx context.Context, source io.Reader, path string, limit int64) (hash string, err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, h), io.LimitReader(buildContextReader{ctx, source}, limit+1))
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", fmt.Errorf("build source too large")
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyBuildSource rejects missing/replaced source on recovery before a machine
// is launched. It never writes a second source copy.
func verifyBuildSource(ctx context.Context, path, expected string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBuildSourceBytes {
		return fmt.Errorf("invalid staged source")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(buildContextReader{ctx, file}, maxBuildSourceBytes+1)); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return ErrSourceHashMismatch
	}
	return nil
}
