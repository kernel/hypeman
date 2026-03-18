package instances

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

const (
	defaultSnapshotCompressionZstdLevel = 1
	maxSnapshotCompressionZstdLevel     = 19
)

type compressionJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type compressionTarget struct {
	Key         string
	OwnerID     string
	SnapshotID  string
	SnapshotDir string
	Policy      snapshotstore.SnapshotCompressionConfig
}

func cloneCompressionConfig(cfg *snapshotstore.SnapshotCompressionConfig) *snapshotstore.SnapshotCompressionConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	if cfg.Level != nil {
		v := *cfg.Level
		cloned.Level = &v
	}
	return &cloned
}

func cloneSnapshotPolicy(policy *SnapshotPolicy) *SnapshotPolicy {
	if policy == nil {
		return nil
	}
	return &SnapshotPolicy{
		Compression: cloneCompressionConfig(policy.Compression),
	}
}

func normalizeCompressionConfig(cfg *snapshotstore.SnapshotCompressionConfig) (snapshotstore.SnapshotCompressionConfig, error) {
	if cfg == nil || !cfg.Enabled {
		return snapshotstore.SnapshotCompressionConfig{Enabled: false}, nil
	}

	normalized := snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: cfg.Algorithm,
	}
	switch normalized.Algorithm {
	case "":
		normalized.Algorithm = snapshotstore.SnapshotCompressionAlgorithmZstd
	case snapshotstore.SnapshotCompressionAlgorithmZstd, snapshotstore.SnapshotCompressionAlgorithmLz4:
	default:
		return snapshotstore.SnapshotCompressionConfig{}, fmt.Errorf("%w: unsupported compression algorithm %q", ErrInvalidRequest, cfg.Algorithm)
	}

	switch normalized.Algorithm {
	case snapshotstore.SnapshotCompressionAlgorithmZstd:
		level := defaultSnapshotCompressionZstdLevel
		if cfg.Level != nil {
			level = *cfg.Level
		}
		if level < 1 || level > maxSnapshotCompressionZstdLevel {
			return snapshotstore.SnapshotCompressionConfig{}, fmt.Errorf("%w: invalid zstd level %d (must be between 1 and %d)", ErrInvalidRequest, level, maxSnapshotCompressionZstdLevel)
		}
		normalized.Level = &level
	case snapshotstore.SnapshotCompressionAlgorithmLz4:
		if cfg.Level != nil {
			return snapshotstore.SnapshotCompressionConfig{}, fmt.Errorf("%w: lz4 does not support level", ErrInvalidRequest)
		}
	}
	return normalized, nil
}

func (m *manager) resolveSnapshotCompressionPolicy(stored *StoredMetadata, override *snapshotstore.SnapshotCompressionConfig) (snapshotstore.SnapshotCompressionConfig, error) {
	if override != nil {
		return normalizeCompressionConfig(override)
	}
	if stored != nil && stored.SnapshotPolicy != nil && stored.SnapshotPolicy.Compression != nil {
		return normalizeCompressionConfig(stored.SnapshotPolicy.Compression)
	}
	if m.snapshotDefaults.Compression != nil {
		return normalizeCompressionConfig(m.snapshotDefaults.Compression)
	}
	return snapshotstore.SnapshotCompressionConfig{Enabled: false}, nil
}

func (m *manager) snapshotJobKeyForInstance(instanceID string) string {
	return "instance:" + instanceID
}

func (m *manager) snapshotJobKeyForSnapshot(snapshotID string) string {
	return "snapshot:" + snapshotID
}

func (m *manager) startCompressionJob(ctx context.Context, target compressionTarget) {
	if target.Key == "" || !target.Policy.Enabled {
		return
	}

	m.compressionMu.Lock()
	if _, exists := m.compressionJobs[target.Key]; exists {
		m.compressionMu.Unlock()
		return
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	job := &compressionJob{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.compressionJobs[target.Key] = job
	m.compressionMu.Unlock()

	go func() {
		defer func() {
			m.compressionMu.Lock()
			delete(m.compressionJobs, target.Key)
			m.compressionMu.Unlock()
			close(job.done)
		}()

		log := logger.FromContext(ctx)
		rawPath, ok := findRawSnapshotMemoryFile(target.SnapshotDir)
		if !ok {
			if _, _, found := findCompressedSnapshotMemoryFile(target.SnapshotDir); found && target.SnapshotID != "" {
				_ = m.updateSnapshotCompressionMetadata(target.SnapshotID, snapshotstore.SnapshotCompressionStateCompressed, "", &target.Policy, nil, nil)
			}
			return
		}

		uncompressedSize, compressedSize, err := compressSnapshotMemoryFile(jobCtx, rawPath, target.Policy)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				if target.SnapshotID != "" {
					_ = m.updateSnapshotCompressionMetadata(target.SnapshotID, snapshotstore.SnapshotCompressionStateNone, "", nil, nil, nil)
				}
				return
			}
			if target.SnapshotID != "" {
				_ = m.updateSnapshotCompressionMetadata(target.SnapshotID, snapshotstore.SnapshotCompressionStateError, err.Error(), &target.Policy, nil, nil)
			}
			log.WarnContext(ctx, "snapshot compression failed", "snapshot_dir", target.SnapshotDir, "error", err)
			return
		}

		if target.SnapshotID != "" {
			_ = m.updateSnapshotCompressionMetadata(target.SnapshotID, snapshotstore.SnapshotCompressionStateCompressed, "", &target.Policy, &compressedSize, &uncompressedSize)
		}
	}()
}

func (m *manager) cancelCompressionJob(key string) {
	m.compressionMu.Lock()
	job := m.compressionJobs[key]
	m.compressionMu.Unlock()
	if job != nil {
		job.cancel()
	}
}

func (m *manager) waitCompressionJob(key string, timeout time.Duration) {
	m.compressionMu.Lock()
	job := m.compressionJobs[key]
	m.compressionMu.Unlock()
	if job == nil {
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-job.done:
	case <-timer.C:
	}
}

func (m *manager) ensureSnapshotMemoryReady(ctx context.Context, snapshotDir, jobKey string) error {
	if jobKey != "" {
		m.cancelCompressionJob(jobKey)
		m.waitCompressionJob(jobKey, 2*time.Second)
	}

	if _, ok := findRawSnapshotMemoryFile(snapshotDir); ok {
		return nil
	}
	compressedPath, algorithm, ok := findCompressedSnapshotMemoryFile(snapshotDir)
	if !ok {
		return nil
	}
	return decompressSnapshotMemoryFile(ctx, compressedPath, algorithm)
}

func (m *manager) updateSnapshotCompressionMetadata(snapshotID, state, compressionError string, cfg *snapshotstore.SnapshotCompressionConfig, compressedSize, uncompressedSize *int64) error {
	rec, err := m.loadSnapshotRecord(snapshotID)
	if err != nil {
		return err
	}
	rec.Snapshot.CompressionState = state
	rec.Snapshot.CompressionError = compressionError
	rec.Snapshot.Compression = cloneCompressionConfig(cfg)
	rec.Snapshot.CompressedSizeBytes = compressedSize
	rec.Snapshot.UncompressedSizeBytes = uncompressedSize

	if state == snapshotstore.SnapshotCompressionStateCompressed {
		sizeBytes, sizeErr := snapshotstore.DirectoryFileSize(m.paths.SnapshotGuestDir(snapshotID))
		if sizeErr == nil {
			rec.Snapshot.SizeBytes = sizeBytes
		}
	}
	return m.saveSnapshotRecord(rec)
}

func findRawSnapshotMemoryFile(snapshotDir string) (string, bool) {
	for _, candidate := range snapshotMemoryFileCandidates(snapshotDir) {
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
			return candidate, true
		}
	}
	return "", false
}

func findCompressedSnapshotMemoryFile(snapshotDir string) (string, snapshotstore.SnapshotCompressionAlgorithm, bool) {
	for _, raw := range snapshotMemoryFileCandidates(snapshotDir) {
		zstdPath := raw + ".zst"
		if st, err := os.Stat(zstdPath); err == nil && st.Mode().IsRegular() {
			return zstdPath, snapshotstore.SnapshotCompressionAlgorithmZstd, true
		}
		lz4Path := raw + ".lz4"
		if st, err := os.Stat(lz4Path); err == nil && st.Mode().IsRegular() {
			return lz4Path, snapshotstore.SnapshotCompressionAlgorithmLz4, true
		}
	}
	return "", "", false
}

func snapshotMemoryFileCandidates(snapshotDir string) []string {
	return []string{
		filepath.Join(snapshotDir, "memory-ranges"),
		filepath.Join(snapshotDir, "memory"),
		filepath.Join(snapshotDir, "snapshots", "snapshot-latest", "memory-ranges"),
		filepath.Join(snapshotDir, "snapshots", "snapshot-latest", "memory"),
	}
}

func compressSnapshotMemoryFile(ctx context.Context, rawPath string, cfg snapshotstore.SnapshotCompressionConfig) (int64, int64, error) {
	rawInfo, err := os.Stat(rawPath)
	if err != nil {
		return 0, 0, fmt.Errorf("stat raw memory snapshot: %w", err)
	}
	uncompressedSize := rawInfo.Size()

	compressedPath := compressedPathFor(rawPath, cfg.Algorithm)
	tmpPath := compressedPath + ".tmp"
	_ = os.Remove(tmpPath)
	_ = os.Remove(compressedPath)

	if err := runCompression(ctx, rawPath, tmpPath, cfg); err != nil {
		_ = os.Remove(tmpPath)
		return 0, 0, err
	}
	if err := os.Rename(tmpPath, compressedPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("finalize compressed snapshot: %w", err)
	}

	compressedInfo, err := os.Stat(compressedPath)
	if err != nil {
		return 0, 0, fmt.Errorf("stat compressed snapshot: %w", err)
	}
	if err := os.Remove(rawPath); err != nil {
		return 0, 0, fmt.Errorf("remove raw memory snapshot: %w", err)
	}
	return uncompressedSize, compressedInfo.Size(), nil
}

func runCompression(ctx context.Context, srcPath, dstPath string, cfg snapshotstore.SnapshotCompressionConfig) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source snapshot: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create compressed snapshot: %w", err)
	}
	defer dst.Close()

	switch cfg.Algorithm {
	case snapshotstore.SnapshotCompressionAlgorithmZstd:
		level := defaultSnapshotCompressionZstdLevel
		if cfg.Level != nil {
			level = *cfg.Level
		}
		enc, err := zstd.NewWriter(dst, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
		if err != nil {
			return fmt.Errorf("create zstd encoder: %w", err)
		}
		if err := copyWithContext(ctx, enc, src); err != nil {
			_ = enc.Close()
			return err
		}
		if err := enc.Close(); err != nil {
			return fmt.Errorf("close zstd encoder: %w", err)
		}
	case snapshotstore.SnapshotCompressionAlgorithmLz4:
		enc := lz4.NewWriter(dst)
		if err := copyWithContext(ctx, enc, src); err != nil {
			_ = enc.Close()
			return err
		}
		if err := enc.Close(); err != nil {
			return fmt.Errorf("close lz4 encoder: %w", err)
		}
	default:
		return fmt.Errorf("%w: unsupported compression algorithm %q", ErrInvalidRequest, cfg.Algorithm)
	}
	return nil
}

func decompressSnapshotMemoryFile(ctx context.Context, compressedPath string, algorithm snapshotstore.SnapshotCompressionAlgorithm) error {
	rawPath := strings.TrimSuffix(strings.TrimSuffix(compressedPath, ".zst"), ".lz4")
	tmpRawPath := rawPath + ".tmp"
	_ = os.Remove(tmpRawPath)

	src, err := os.Open(compressedPath)
	if err != nil {
		return fmt.Errorf("open compressed snapshot: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(tmpRawPath)
	if err != nil {
		return fmt.Errorf("create decompressed snapshot file: %w", err)
	}
	defer dst.Close()

	var reader io.Reader
	switch algorithm {
	case snapshotstore.SnapshotCompressionAlgorithmZstd:
		dec, err := zstd.NewReader(src)
		if err != nil {
			return fmt.Errorf("create zstd decoder: %w", err)
		}
		defer dec.Close()
		reader = dec
	case snapshotstore.SnapshotCompressionAlgorithmLz4:
		reader = lz4.NewReader(src)
	default:
		return fmt.Errorf("%w: unsupported compression algorithm %q", ErrInvalidRequest, algorithm)
	}

	if err := copyWithContext(ctx, dst, reader); err != nil {
		_ = os.Remove(tmpRawPath)
		return err
	}
	if err := os.Rename(tmpRawPath, rawPath); err != nil {
		_ = os.Remove(tmpRawPath)
		return fmt.Errorf("finalize decompressed snapshot: %w", err)
	}
	return nil
}

func compressedPathFor(rawPath string, algorithm snapshotstore.SnapshotCompressionAlgorithm) string {
	switch algorithm {
	case snapshotstore.SnapshotCompressionAlgorithmLz4:
		return rawPath + ".lz4"
	default:
		return rawPath + ".zst"
	}
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write compressed stream: %w", writeErr)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("read snapshot stream: %w", readErr)
		}
	}
}
