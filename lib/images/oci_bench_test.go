package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
)

// BenchmarkUnpackMethods compares umoci-based unpacking vs streaming tar extraction.
// This benchmark helps measure the performance improvement of the streaming approach.
//
// Run with: go test -bench=BenchmarkUnpackMethods -benchtime=5s ./lib/images/
//
// Note: This benchmark uses a synthetic test image. For more realistic benchmarks,
// use the E2E benchmark script with real images from a local registry.
func BenchmarkUnpackMethods(b *testing.B) {
	// Create synthetic Docker image once (shared across sub-benchmarks)
	img := createTestDockerImageForBench(b)

	imgDigest, err := img.Digest()
	require.NoError(b, err)
	digestStr := imgDigest.String()
	layoutTag := digestToLayoutTag(digestStr)

	// Create OCI layout cache (shared setup)
	cacheDir := b.TempDir()
	path, err := layout.Write(cacheDir, empty.Index)
	require.NoError(b, err)

	err = path.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": layoutTag,
	}))
	require.NoError(b, err)

	client, err := newOCIClient(cacheDir)
	require.NoError(b, err)

	ctx := context.Background()

	b.Run("umoci", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			targetDir := b.TempDir()
			err := client.unpackLayers(ctx, layoutTag, targetDir)
			require.NoError(b, err)
		}
	})

	// Note: streamingUnpack requires a real registry to pull from,
	// so we can only benchmark it in integration tests or E2E.
	// Here we benchmark the tar extraction portion as a proxy.
	b.Run("extractTarStream", func(b *testing.B) {
		// Get layer reader from cached image
		layoutPath, _ := layout.FromPath(cacheDir)
		cachedImg, _ := imageByAnnotation(layoutPath, layoutTag)
		layers, _ := cachedImg.Layers()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			targetDir := b.TempDir()
			for _, layer := range layers {
				reader, _ := layer.Uncompressed()
				err := extractTarStream(ctx, reader, targetDir)
				reader.Close()
				require.NoError(b, err)
			}
		}
	})
}

// BenchmarkUnpackMethodsLarge compares unpacking methods with a larger, more realistic image.
// This image has ~1000 files and ~10MB of content, similar to a small application image.
//
// Run with: go test -bench=BenchmarkUnpackMethodsLarge -benchtime=10s ./lib/images/
func BenchmarkUnpackMethodsLarge(b *testing.B) {
	// Create a larger synthetic image
	img := createLargeTestImage(b, 1000, 10*1024) // 1000 files, 10KB each = ~10MB

	imgDigest, err := img.Digest()
	require.NoError(b, err)
	digestStr := imgDigest.String()
	layoutTag := digestToLayoutTag(digestStr)

	// Create OCI layout cache
	cacheDir := b.TempDir()
	path, err := layout.Write(cacheDir, empty.Index)
	require.NoError(b, err)

	err = path.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": layoutTag,
	}))
	require.NoError(b, err)

	client, err := newOCIClient(cacheDir)
	require.NoError(b, err)

	ctx := context.Background()

	// Report image size
	layers, _ := img.Layers()
	var totalSize int64
	for _, layer := range layers {
		size, _ := layer.Size()
		totalSize += size
	}
	b.Logf("Image size: %d bytes (compressed), %d files", totalSize, 1000)

	b.Run("umoci", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			targetDir := b.TempDir()
			err := client.unpackLayers(ctx, layoutTag, targetDir)
			require.NoError(b, err)
		}
	})

	b.Run("extractTarStream", func(b *testing.B) {
		layoutPath, _ := layout.FromPath(cacheDir)
		cachedImg, _ := imageByAnnotation(layoutPath, layoutTag)
		layers, _ := cachedImg.Layers()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			targetDir := b.TempDir()
			for _, layer := range layers {
				reader, _ := layer.Uncompressed()
				err := extractTarStream(ctx, reader, targetDir)
				reader.Close()
				require.NoError(b, err)
			}
		}
	})
}

// BenchmarkUnpackMethodsVeryLarge tests with a ~50MB image (closer to real app images).
// Run with: go test -bench=BenchmarkUnpackMethodsVeryLarge -benchtime=30s ./lib/images/
func BenchmarkUnpackMethodsVeryLarge(b *testing.B) {
	// Create a ~50MB image: 2000 files x 25KB each
	img := createLargeTestImage(b, 2000, 25*1024)

	imgDigest, err := img.Digest()
	require.NoError(b, err)
	digestStr := imgDigest.String()
	layoutTag := digestToLayoutTag(digestStr)

	cacheDir := b.TempDir()
	path, err := layout.Write(cacheDir, empty.Index)
	require.NoError(b, err)

	err = path.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": layoutTag,
	}))
	require.NoError(b, err)

	client, err := newOCIClient(cacheDir)
	require.NoError(b, err)

	ctx := context.Background()

	b.Run("umoci", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			targetDir := b.TempDir()
			err := client.unpackLayers(ctx, layoutTag, targetDir)
			require.NoError(b, err)
		}
	})

	b.Run("extractTarStream", func(b *testing.B) {
		layoutPath, _ := layout.FromPath(cacheDir)
		cachedImg, _ := imageByAnnotation(layoutPath, layoutTag)
		layers, _ := cachedImg.Layers()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			targetDir := b.TempDir()
			for _, layer := range layers {
				reader, _ := layer.Uncompressed()
				err := extractTarStream(ctx, reader, targetDir)
				reader.Close()
				require.NoError(b, err)
			}
		}
	})
}

// createLargeTestImage creates a synthetic image with many files for benchmarking.
// numFiles: number of files to create
// fileSize: size of each file in bytes
func createLargeTestImage(b *testing.B, numFiles int, fileSize int) v1.Image {
	b.Helper()

	// Generate random content for files (same content reused for compression efficiency)
	fileContent := make([]byte, fileSize)
	for i := range fileContent {
		fileContent[i] = byte(i % 256)
	}

	// Build a gzipped tar layer with many files
	var layerBuf bytes.Buffer
	gzw := gzip.NewWriter(&layerBuf)
	tw := tar.NewWriter(gzw)

	// Create directory structure
	dirs := []string{"app/", "app/src/", "app/lib/", "app/data/", "app/config/"}
	for _, dir := range dirs {
		require.NoError(b, tw.WriteHeader(&tar.Header{
			Name:     dir,
			Typeflag: tar.TypeDir,
			Mode:     0755,
		}))
	}

	// Create files distributed across directories
	for i := 0; i < numFiles; i++ {
		dirIdx := i % len(dirs)
		fileName := fmt.Sprintf("%sfile_%04d.dat", dirs[dirIdx], i)

		require.NoError(b, tw.WriteHeader(&tar.Header{
			Name:     fileName,
			Size:     int64(fileSize),
			Typeflag: tar.TypeReg,
			Mode:     0644,
		}))
		_, err := tw.Write(fileContent)
		require.NoError(b, err)
	}

	require.NoError(b, tw.Close())
	require.NoError(b, gzw.Close())

	layerBytes := layerBuf.Bytes()
	b.Logf("Layer size: %d bytes (compressed from %d files x %d bytes)", len(layerBytes), numFiles, fileSize)

	// Create layer from bytes
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerBytes)), nil
	})
	require.NoError(b, err)

	// Build image with layer
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(b, err)

	img, err = mutate.Config(img, v1.Config{
		Entrypoint: []string{"/app/main"},
		WorkingDir: "/app",
	})
	require.NoError(b, err)

	return img
}

// BenchmarkProcessWhiteouts measures the overhead of whiteout processing.
func BenchmarkProcessWhiteouts(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Setup: create a directory with files and whiteouts
		targetDir := b.TempDir()
		for j := 0; j < 100; j++ {
			os.WriteFile(targetDir+"/file"+string(rune(j))+".txt", []byte("content"), 0644)
		}
		// Add some whiteouts
		os.WriteFile(targetDir+"/.wh.file10.txt", []byte{}, 0644)
		os.WriteFile(targetDir+"/.wh.file20.txt", []byte{}, 0644)
		os.WriteFile(targetDir+"/.wh.file30.txt", []byte{}, 0644)
		b.StartTimer()

		err := processWhiteouts(targetDir)
		require.NoError(b, err)
	}
}

// createTestDockerImageForBench creates a synthetic Docker image for benchmarking.
// This is a copy of createTestDockerImage adapted for *testing.B.
func createTestDockerImageForBench(b *testing.B) v1.Image {
	b.Helper()

	// Build a gzipped tar layer with test files
	var layerBuf bytes.Buffer
	gzw := gzip.NewWriter(&layerBuf)
	tw := tar.NewWriter(gzw)

	files := []struct {
		name    string
		content string
		mode    int64
		isDir   bool
	}{
		{name: "usr/", isDir: true, mode: 0755},
		{name: "usr/local/", isDir: true, mode: 0755},
		{name: "usr/local/bin/", isDir: true, mode: 0755},
		{name: "usr/local/bin/guest-agent", content: "fake-builder-binary-v1", mode: 0755},
		{name: "etc/", isDir: true, mode: 0755},
		{name: "etc/builder.json", content: `{"version":"1.0"}`, mode: 0644},
		{name: "app/", isDir: true, mode: 0755},
	}

	for _, f := range files {
		if f.isDir {
			require.NoError(b, tw.WriteHeader(&tar.Header{
				Name:     f.name,
				Typeflag: tar.TypeDir,
				Mode:     f.mode,
			}))
		} else {
			require.NoError(b, tw.WriteHeader(&tar.Header{
				Name:     f.name,
				Size:     int64(len(f.content)),
				Typeflag: tar.TypeReg,
				Mode:     f.mode,
			}))
			_, err := tw.Write([]byte(f.content))
			require.NoError(b, err)
		}
	}
	require.NoError(b, tw.Close())
	require.NoError(b, gzw.Close())

	layerBytes := layerBuf.Bytes()

	// Create layer from bytes
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerBytes)), nil
	})
	require.NoError(b, err)

	// Start with empty image and add our layer
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(b, err)

	// Set config (entrypoint, env, workdir)
	img, err = mutate.Config(img, v1.Config{
		Entrypoint: []string{"/usr/local/bin/guest-agent"},
		Env:        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		WorkingDir: "/app",
	})
	require.NoError(b, err)

	return img
}
