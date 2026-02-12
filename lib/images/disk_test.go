package images

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestExportRootfsFormats exercises ext4 and erofs export with a realistic
// rootfs directory and compares creation time and output size.
func TestExportRootfsFormats(t *testing.T) {
	// Build a small but representative rootfs tree
	rootfsDir := t.TempDir()
	populateTestRootfs(t, rootfsDir)

	formats := []ExportFormat{FormatExt4, FormatErofs}
	results := make(map[ExportFormat]struct {
		size     int64
		duration time.Duration
	})

	for _, fmt := range formats {
		t.Run(string(fmt), func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "rootfs."+string(fmt))

			start := time.Now()
			size, err := ExportRootfs(rootfsDir, outPath, fmt)
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("ExportRootfs(%s) failed: %v", fmt, err)
			}
			if size == 0 {
				t.Fatalf("ExportRootfs(%s) returned zero size", fmt)
			}

			// Verify file exists and matches reported size
			stat, err := os.Stat(outPath)
			if err != nil {
				t.Fatalf("stat output: %v", err)
			}
			if stat.Size() != size {
				t.Errorf("reported size %d != actual size %d", size, stat.Size())
			}

			// Verify sector alignment (4096 bytes)
			if size%4096 != 0 {
				t.Errorf("output size %d is not sector-aligned (4096)", size)
			}

			t.Logf("%s: size=%d bytes (%.1f KB), time=%v", fmt, size, float64(size)/1024, elapsed)
			results[fmt] = struct {
				size     int64
				duration time.Duration
			}{size, elapsed}
		})
	}

	// Log comparison if both ran
	if ext4, ok := results[FormatExt4]; ok {
		if erofs, ok := results[FormatErofs]; ok {
			ratio := float64(erofs.size) / float64(ext4.size) * 100
			t.Logf("erofs is %.0f%% the size of ext4 (ext4=%d, erofs=%d)",
				ratio, ext4.size, erofs.size)
		}
	}
}

// populateTestRootfs creates a small rootfs structure that resembles a real
// container image (directories, binaries, text files, symlinks).
func populateTestRootfs(t *testing.T, dir string) {
	t.Helper()

	dirs := []string{
		"bin", "etc", "usr/bin", "usr/lib", "var/log", "tmp",
		"boot-node/app/dist", "boot-node/app/node_modules/.pnpm",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Simulate typical files: configs, scripts, compiled JS
	files := map[string]int{
		"etc/passwd":                          256,
		"etc/hostname":                        16,
		"bin/sh":                              128 * 1024, // 128KB "binary"
		"usr/bin/node":                        512 * 1024, // 512KB "binary"
		"boot-node/app/package.json":          1024,
		"boot-node/app/dist/index.js":         32 * 1024,
		"boot-node/app/node_modules/.pnpm/a":  64 * 1024,
		"boot-node/app/node_modules/.pnpm/b":  64 * 1024,
		"boot-node/app/node_modules/.pnpm/c":  64 * 1024,
		"var/log/boot.log":                    0,
	}
	for name, size := range files {
		data := make([]byte, size)
		// Fill with non-zero data so compression has something to work with
		for i := range data {
			data[i] = byte(i % 251)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Symlink
	os.Symlink("sh", filepath.Join(dir, "bin/bash"))
}
