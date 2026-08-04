package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneOldInitrdBuilds(t *testing.T) {
	initrdDir := filepath.Join(t.TempDir(), "system", "initrd", "x86_64")
	cutoff := time.Now().Add(-2 * time.Hour)

	createBuild := func(name string, modTime time.Time) string {
		t.Helper()
		buildDir := filepath.Join(initrdDir, name)
		if err := os.MkdirAll(buildDir, 0755); err != nil {
			t.Fatal(err)
		}
		initrdPath := filepath.Join(buildDir, "initrd")
		if err := os.WriteFile(initrdPath, []byte("initrd"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(buildDir, modTime, modTime); err != nil {
			t.Fatal(err)
		}
		return initrdPath
	}

	oldBuild := createBuild("100", cutoff.Add(-time.Hour))
	currentBuild := createBuild("200", cutoff.Add(-time.Hour))
	latestBuild := createBuild("300", cutoff.Add(-time.Hour))
	recentBuild := createBuild("400", cutoff.Add(time.Hour))
	nonnumericBuild := createBuild("manual", cutoff.Add(-time.Hour))
	if err := os.Symlink(filepath.Base(filepath.Dir(latestBuild)), filepath.Join(initrdDir, "latest")); err != nil {
		t.Fatal(err)
	}

	pruned, err := pruneOldInitrdBuilds(currentBuild, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d builds, want 1", pruned)
	}
	if _, err := os.Stat(oldBuild); !os.IsNotExist(err) {
		t.Fatalf("old build still exists: %v", err)
	}
	for _, path := range []string{currentBuild, latestBuild, recentBuild, nonnumericBuild} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to remain: %v", path, err)
		}
	}
}
