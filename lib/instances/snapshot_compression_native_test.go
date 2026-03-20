package instances

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type codecLogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type codecLogRecorder struct {
	mu      sync.Mutex
	records []codecLogRecord
}

func (h *codecLogRecorder) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *codecLogRecorder) Handle(_ context.Context, r slog.Record) error {
	rec := codecLogRecord{
		level: r.Level,
		msg:   r.Message,
		attrs: map[string]string{},
	}
	r.Attrs(func(attr slog.Attr) bool {
		rec.attrs[attr.Key] = attr.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *codecLogRecorder) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *codecLogRecorder) WithGroup(_ string) slog.Handler      { return h }

func (h *codecLogRecorder) warnRecords() []codecLogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]codecLogRecord, 0, len(h.records))
	for _, rec := range h.records {
		if rec.level == slog.LevelWarn {
			out = append(out, rec)
		}
	}
	return out
}

func TestNativeCompressionArgs(t *testing.T) {
	t.Parallel()

	zstdArgs := nativeCompressionArgs("/tmp/raw", "/tmp/raw.zst.tmp", snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(19),
	})
	assert.Equal(t, []string{"-q", "-f", "-19", "-o", "/tmp/raw.zst.tmp", "/tmp/raw"}, zstdArgs)

	lz4Args := nativeCompressionArgs("/tmp/raw", "/tmp/raw.lz4.tmp", snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
		Level:     intPtr(0),
	})
	assert.Equal(t, []string{"-q", "-f", "--fast=1", "/tmp/raw", "/tmp/raw.lz4.tmp"}, lz4Args)

	lz4Args = nativeCompressionArgs("/tmp/raw", "/tmp/raw.lz4.tmp", snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
		Level:     intPtr(9),
	})
	assert.Equal(t, []string{"-q", "-f", "-9", "/tmp/raw", "/tmp/raw.lz4.tmp"}, lz4Args)
}

func TestNativeDecompressionArgs(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		[]string{"-q", "-d", "-f", "-o", "/tmp/raw.tmp", "/tmp/raw.zst"},
		nativeDecompressionArgs("/tmp/raw.zst", "/tmp/raw.tmp", snapshotstore.SnapshotCompressionAlgorithmZstd),
	)
	assert.Equal(t,
		[]string{"-q", "-d", "-f", "/tmp/raw.lz4", "/tmp/raw.tmp"},
		nativeDecompressionArgs("/tmp/raw.lz4", "/tmp/raw.tmp", snapshotstore.SnapshotCompressionAlgorithmLz4),
	)
}

func TestIsNativeCodecUnavailable(t *testing.T) {
	t.Parallel()

	reason, ok := isNativeCodecUnavailable(exec.ErrNotFound)
	require.True(t, ok)
	assert.Equal(t, snapshotCodecFallbackReasonMissingBinary, reason)

	reason, ok = isNativeCodecUnavailable(&os.PathError{Op: "fork/exec", Path: "/missing/zstd", Err: syscall.ENOENT})
	require.True(t, ok)
	assert.Equal(t, snapshotCodecFallbackReasonMissingBinary, reason)

	reason, ok = isNativeCodecUnavailable(&os.PathError{Op: "fork/exec", Path: "/usr/bin/zstd", Err: syscall.EACCES})
	require.True(t, ok)
	assert.Equal(t, snapshotCodecFallbackReasonNotExecutable, reason)

	_, ok = isNativeCodecUnavailable(assert.AnError)
	assert.False(t, ok)
}

func TestResolveNativeCodecPathCachesSuccess(t *testing.T) {
	t.Parallel()

	mgr := &manager{nativeCodecPaths: make(map[string]string)}
	lookups := 0
	runtime := nativeCodecRuntime{
		manager: mgr,
		lookPath: func(file string) (string, error) {
			lookups++
			return "/usr/bin/" + file, nil
		},
	}

	path, err := resolveNativeCodecPath(runtime, snapshotstore.SnapshotCompressionAlgorithmZstd)
	require.NoError(t, err)
	assert.Equal(t, "/usr/bin/zstd", path)

	path, err = resolveNativeCodecPath(runtime, snapshotstore.SnapshotCompressionAlgorithmZstd)
	require.NoError(t, err)
	assert.Equal(t, "/usr/bin/zstd", path)
	assert.Equal(t, 1, lookups)
}

func TestResolveNativeCodecPathDoesNotCacheMiss(t *testing.T) {
	t.Parallel()

	mgr := &manager{nativeCodecPaths: make(map[string]string)}
	lookups := 0
	runtime := nativeCodecRuntime{
		manager: mgr,
		lookPath: func(file string) (string, error) {
			lookups++
			return "", exec.ErrNotFound
		},
	}

	_, err := resolveNativeCodecPath(runtime, snapshotstore.SnapshotCompressionAlgorithmLz4)
	require.Error(t, err)
	_, err = resolveNativeCodecPath(runtime, snapshotstore.SnapshotCompressionAlgorithmLz4)
	require.Error(t, err)
	assert.Equal(t, 2, lookups)
}

func TestCompressSnapshotMemoryFileFallsBackToGoWhenNativeBinaryMissing(t *testing.T) {
	mgr, reader := newCodecRuntimeTestManager(t)
	ctx, logs := newCodecRuntimeContext()
	rawPath, original := writeRawSnapshotMemoryFile(t)

	_, compressedSize, err := compressSnapshotMemoryFileWithRuntime(ctx, nativeCodecRuntime{
		manager: mgr,
		lookPath: func(file string) (string, error) {
			return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
		},
	}, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.NoError(t, err)
	assert.Greater(t, compressedSize, int64(0))

	compressedPath := compressedPathFor(rawPath, snapshotstore.SnapshotCompressionAlgorithmZstd)
	_, statErr := os.Stat(compressedPath)
	require.NoError(t, statErr)
	_, statErr = os.Stat(rawPath)
	assert.Error(t, statErr)

	warns := logs.warnRecords()
	require.Len(t, warns, 1)
	assert.Equal(t, "native snapshot codec unavailable, falling back to go implementation", warns[0].msg)
	assert.Equal(t, "zstd", warns[0].attrs["algorithm"])
	assert.Equal(t, "compress", warns[0].attrs["operation"])
	assert.Equal(t, "zstd", warns[0].attrs["native_binary"])
	assert.Equal(t, "go", warns[0].attrs["fallback_backend"])
	assert.Equal(t, nativeCodecInstallTip(snapshotstore.SnapshotCompressionAlgorithmZstd), warns[0].attrs["install_tip"])

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	fallbackMetric := findMetric(t, rm, "hypeman_snapshot_codec_fallbacks_total")
	fallbacks, ok := fallbackMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, fallbacks.DataPoints, 1)
	assert.Equal(t, int64(1), fallbacks.DataPoints[0].Value)
	assert.Equal(t, "zstd", metricLabel(t, fallbacks.DataPoints[0].Attributes, "algorithm"))
	assert.Equal(t, "compress", metricLabel(t, fallbacks.DataPoints[0].Attributes, "operation"))
	assert.Equal(t, "missing_binary", metricLabel(t, fallbacks.DataPoints[0].Attributes, "reason"))

	runtimeGo := nativeCodecRuntime{lookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	require.NoError(t, decompressSnapshotMemoryFileWithRuntime(context.Background(), runtimeGo, compressedPath, snapshotstore.SnapshotCompressionAlgorithmZstd))
	restored, err := os.ReadFile(rawPath)
	require.NoError(t, err)
	assert.Equal(t, original, restored)
}

func TestCompressSnapshotMemoryFileFallsBackToGoWhenNativeBinaryMissingFromPATH(t *testing.T) {
	mgr, reader := newCodecRuntimeTestManager(t)
	ctx, logs := newCodecRuntimeContext()
	rawPath, _ := writeRawSnapshotMemoryFile(t)
	t.Setenv("PATH", t.TempDir())

	_, compressedSize, err := compressSnapshotMemoryFileWithRuntime(ctx, nativeCodecRuntime{
		manager: mgr,
	}, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.NoError(t, err)
	assert.Greater(t, compressedSize, int64(0))

	warns := logs.warnRecords()
	require.Len(t, warns, 1)
	assert.Equal(t, "zstd", warns[0].attrs["algorithm"])
	assert.Equal(t, "compress", warns[0].attrs["operation"])
	assert.Equal(t, "zstd", warns[0].attrs["native_binary"])

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	fallbackMetric := findMetric(t, rm, "hypeman_snapshot_codec_fallbacks_total")
	fallbacks, ok := fallbackMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, fallbacks.DataPoints, 1)
	assert.Equal(t, int64(1), fallbacks.DataPoints[0].Value)
}

func TestDecompressSnapshotMemoryFileFallsBackToGoWhenNativeBinaryMissing(t *testing.T) {
	mgr, reader := newCodecRuntimeTestManager(t)
	ctx, logs := newCodecRuntimeContext()
	rawPath, original := writeRawSnapshotMemoryFile(t)

	_, _, err := compressSnapshotMemoryFileWithRuntime(context.Background(), nativeCodecRuntime{
		lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
	}, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
		Level:     intPtr(0),
	})
	require.NoError(t, err)

	compressedPath := compressedPathFor(rawPath, snapshotstore.SnapshotCompressionAlgorithmLz4)
	require.NoError(t, decompressSnapshotMemoryFileWithRuntime(ctx, nativeCodecRuntime{
		manager: mgr,
		lookPath: func(file string) (string, error) {
			return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
		},
	}, compressedPath, snapshotstore.SnapshotCompressionAlgorithmLz4))

	restored, err := os.ReadFile(rawPath)
	require.NoError(t, err)
	assert.Equal(t, original, restored)

	warns := logs.warnRecords()
	require.Len(t, warns, 1)
	assert.Equal(t, "lz4", warns[0].attrs["algorithm"])
	assert.Equal(t, "decompress", warns[0].attrs["operation"])
	assert.Equal(t, "lz4", warns[0].attrs["native_binary"])
	assert.Equal(t, "go", warns[0].attrs["fallback_backend"])
	assert.Equal(t, nativeCodecInstallTip(snapshotstore.SnapshotCompressionAlgorithmLz4), warns[0].attrs["install_tip"])

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	fallbackMetric := findMetric(t, rm, "hypeman_snapshot_codec_fallbacks_total")
	fallbacks, ok := fallbackMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, fallbacks.DataPoints, 1)
	assert.Equal(t, int64(1), fallbacks.DataPoints[0].Value)
	assert.Equal(t, "lz4", metricLabel(t, fallbacks.DataPoints[0].Attributes, "algorithm"))
	assert.Equal(t, "decompress", metricLabel(t, fallbacks.DataPoints[0].Attributes, "operation"))
	assert.Equal(t, "missing_binary", metricLabel(t, fallbacks.DataPoints[0].Attributes, "reason"))
}

func TestCompressSnapshotMemoryFileDoesNotFallBackOnNativeRuntimeError(t *testing.T) {
	mgr, reader := newCodecRuntimeTestManager(t)
	ctx, logs := newCodecRuntimeContext()
	rawPath, _ := writeRawSnapshotMemoryFile(t)
	binaryPath := writeExecutableScript(t, "zstd", "#!/bin/sh\necho native boom >&2\nexit 1\n")

	_, _, err := compressSnapshotMemoryFileWithRuntime(ctx, nativeCodecRuntime{
		manager:  mgr,
		lookPath: func(string) (string, error) { return binaryPath, nil },
	}, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "native zstd compress")

	_, statErr := os.Stat(compressedPathFor(rawPath, snapshotstore.SnapshotCompressionAlgorithmZstd))
	assert.Error(t, statErr)
	assert.Empty(t, logs.warnRecords())
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
}

func TestCompressSnapshotMemoryFileReturnsContextCanceledWhenNativeProcessIsKilled(t *testing.T) {
	mgr, reader := newCodecRuntimeTestManager(t)
	baseCtx, logs := newCodecRuntimeContext()
	ctx, cancel := context.WithCancel(baseCtx)
	rawPath, _ := writeRawSnapshotMemoryFile(t)
	binaryPath := writeExecutableScript(t, "zstd", "#!/bin/sh\nsleep 30\n")
	time.AfterFunc(20*time.Millisecond, cancel)

	_, _, err := compressSnapshotMemoryFileWithRuntime(ctx, nativeCodecRuntime{
		manager:  mgr,
		lookPath: func(string) (string, error) { return binaryPath, nil },
	}, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, logs.warnRecords())

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
}

func TestDecompressSnapshotMemoryFileDoesNotFallBackOnNativeRuntimeError(t *testing.T) {
	mgr, reader := newCodecRuntimeTestManager(t)
	ctx, logs := newCodecRuntimeContext()
	rawPath, original := writeRawSnapshotMemoryFile(t)

	_, _, err := compressSnapshotMemoryFileWithRuntime(context.Background(), nativeCodecRuntime{
		lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
	}, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.NoError(t, err)

	compressedPath := compressedPathFor(rawPath, snapshotstore.SnapshotCompressionAlgorithmZstd)
	binaryPath := writeExecutableScript(t, "zstd", "#!/bin/sh\necho native boom >&2\nexit 1\n")
	err = decompressSnapshotMemoryFileWithRuntime(ctx, nativeCodecRuntime{
		manager:  mgr,
		lookPath: func(string) (string, error) { return binaryPath, nil },
	}, compressedPath, snapshotstore.SnapshotCompressionAlgorithmZstd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "native zstd decompress")

	_, statErr := os.Stat(rawPath)
	assert.Error(t, statErr)
	compressedBytes, readErr := os.ReadFile(compressedPath)
	require.NoError(t, readErr)
	assert.NotEmpty(t, compressedBytes)
	assert.NotEqual(t, original, compressedBytes)
	assert.Empty(t, logs.warnRecords())

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
}

func TestCompressSnapshotMemoryFileInvalidatesStaleNativeCodecCache(t *testing.T) {
	t.Parallel()

	mgr := &manager{nativeCodecPaths: map[string]string{"zstd": "/missing/zstd"}}
	rawPath, _ := writeRawSnapshotMemoryFile(t)

	_, _, err := compressSnapshotMemoryFileWithRuntime(context.Background(), nativeCodecRuntime{
		manager:  mgr,
		lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
	}, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.NoError(t, err)
	assert.Empty(t, mgr.nativeCodecPaths)
}

func TestNativeCompressGoDecompressCompatibility(t *testing.T) {
	for _, tc := range compressionCompatibilityCases() {
		t.Run(string(tc.algorithm), func(t *testing.T) {
			binaryPath, err := exec.LookPath(nativeCodecBinaryName(tc.algorithm))
			if err != nil {
				t.Skipf("native %s binary not available: %v", tc.algorithm, err)
			}

			rawPath, original := writeRawSnapshotMemoryFile(t)
			_, _, err = compressSnapshotMemoryFileWithRuntime(context.Background(), nativeCodecRuntime{
				lookPath: func(string) (string, error) { return binaryPath, nil },
			}, rawPath, tc.cfg)
			require.NoError(t, err)

			compressedPath := compressedPathFor(rawPath, tc.algorithm)
			require.NoError(t, decompressSnapshotMemoryFileWithRuntime(context.Background(), nativeCodecRuntime{
				lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
			}, compressedPath, tc.algorithm))

			restored, err := os.ReadFile(rawPath)
			require.NoError(t, err)
			assert.Equal(t, original, restored)
		})
	}
}

func TestGoCompressNativeDecompressCompatibility(t *testing.T) {
	for _, tc := range compressionCompatibilityCases() {
		t.Run(string(tc.algorithm), func(t *testing.T) {
			binaryPath, err := exec.LookPath(nativeCodecBinaryName(tc.algorithm))
			if err != nil {
				t.Skipf("native %s binary not available: %v", tc.algorithm, err)
			}

			rawPath, original := writeRawSnapshotMemoryFile(t)
			_, _, err = compressSnapshotMemoryFileWithRuntime(context.Background(), nativeCodecRuntime{
				lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
			}, rawPath, tc.cfg)
			require.NoError(t, err)

			compressedPath := compressedPathFor(rawPath, tc.algorithm)
			require.NoError(t, decompressSnapshotMemoryFileWithRuntime(context.Background(), nativeCodecRuntime{
				lookPath: func(string) (string, error) { return binaryPath, nil },
			}, compressedPath, tc.algorithm))

			restored, err := os.ReadFile(rawPath)
			require.NoError(t, err)
			assert.Equal(t, original, restored)
		})
	}
}

func TestNativeCompressNativeDecompressRoundTrip(t *testing.T) {
	for _, tc := range compressionCompatibilityCases() {
		t.Run(string(tc.algorithm), func(t *testing.T) {
			binaryPath, err := exec.LookPath(nativeCodecBinaryName(tc.algorithm))
			if err != nil {
				t.Skipf("native %s binary not available: %v", tc.algorithm, err)
			}

			rawPath, original := writeRawSnapshotMemoryFile(t)
			runtime := nativeCodecRuntime{lookPath: func(string) (string, error) { return binaryPath, nil }}
			_, _, err = compressSnapshotMemoryFileWithRuntime(context.Background(), runtime, rawPath, tc.cfg)
			require.NoError(t, err)
			compressedPath := compressedPathFor(rawPath, tc.algorithm)
			require.NoError(t, decompressSnapshotMemoryFileWithRuntime(context.Background(), runtime, compressedPath, tc.algorithm))

			restored, err := os.ReadFile(rawPath)
			require.NoError(t, err)
			assert.Equal(t, original, restored)
		})
	}
}

type compressionCompatibilityCase struct {
	algorithm snapshotstore.SnapshotCompressionAlgorithm
	cfg       snapshotstore.SnapshotCompressionConfig
}

func compressionCompatibilityCases() []compressionCompatibilityCase {
	return []compressionCompatibilityCase{
		{
			algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
			cfg:       snapshotstore.SnapshotCompressionConfig{Enabled: true, Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd, Level: intPtr(1)},
		},
		{
			algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
			cfg:       snapshotstore.SnapshotCompressionConfig{Enabled: true, Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4, Level: intPtr(0)},
		},
	}
}

func newCodecRuntimeTestManager(t *testing.T) (*manager, *otelmetric.ManualReader) {
	t.Helper()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))
	mgr := &manager{
		paths:            paths.New(t.TempDir()),
		compressionJobs:  make(map[string]*compressionJob),
		nativeCodecPaths: make(map[string]string),
	}
	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, mgr)
	require.NoError(t, err)
	mgr.metrics = metrics
	return mgr, reader
}

func newCodecRuntimeContext() (context.Context, *codecLogRecorder) {
	recorder := &codecLogRecorder{}
	log := slog.New(recorder)
	return logger.AddToContext(context.Background(), log), recorder
}

func writeRawSnapshotMemoryFile(t *testing.T) (string, []byte) {
	t.Helper()

	dir := t.TempDir()
	rawPath := filepath.Join(dir, "memory-ranges")
	content := []byte("snapshot-memory-contents-for-native-codec-tests")
	require.NoError(t, os.WriteFile(rawPath, content, 0o644))
	return rawPath, content
}

func writeExecutableScript(t *testing.T, name, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}
