package firecracker

import (
	"fmt"
	"os"
	"path/filepath"
)

func prepareSnapshotDataDirForFork(snapshotConfigPath string, meta *restoreMetadata, sourceDir, targetDir string) (bool, error) {
	statePath := snapshotStatePath(filepath.Dir(snapshotConfigPath))
	rewritten, err := rewriteSnapshotStatePathsForFork(statePath, snapshotStatePathRewritesForFork(meta, sourceDir, targetDir))
	if err != nil {
		return false, err
	}
	if rewritten {
		return clearSnapshotSourceAlias(meta), nil
	}
	return recordSnapshotSourceAliasFallback(meta, sourceDir)
}

func clearSnapshotSourceAlias(meta *restoreMetadata) bool {
	changed := false
	if meta.SnapshotSourceDataDir != "" {
		meta.SnapshotSourceDataDir = ""
		changed = true
	}
	if meta.RetainSnapshotSourceDataDirAlias {
		meta.RetainSnapshotSourceDataDirAlias = false
		changed = true
	}
	return changed
}

func recordSnapshotSourceAliasFallback(meta *restoreMetadata, sourceDir string) (bool, error) {
	if meta.RetainSnapshotSourceDataDirAlias && meta.SnapshotSourceDataDir != "" {
		// Keep the upstream source path for snapshot-derived forks. The retained
		// Firecracker base can still reference that path after later diff snapshots.
		return false, nil
	}

	retainAlias := false
	if _, err := os.Stat(sourceDir); err != nil {
		if os.IsNotExist(err) {
			retainAlias = true
		} else {
			return false, fmt.Errorf("stat snapshot source data dir %q: %w", sourceDir, err)
		}
	}

	changed := false
	if meta.SnapshotSourceDataDir != sourceDir {
		meta.SnapshotSourceDataDir = sourceDir
		changed = true
	}
	if meta.RetainSnapshotSourceDataDirAlias != retainAlias {
		meta.RetainSnapshotSourceDataDirAlias = retainAlias
		changed = true
	}
	return changed, nil
}

func snapshotStatePathRewritesForFork(meta *restoreMetadata, sourceDataDir, targetDataDir string) []snapshotStatePathRewrite {
	var rewrites []snapshotStatePathRewrite
	add := func(source, target string) {
		if source == "" || target == "" {
			return
		}
		rewrites = append(rewrites, snapshotStatePathRewrite{Source: source, Target: target})
	}

	add(sourceDataDir, targetDataDir)
	if meta != nil {
		add(meta.SnapshotSourceDataDir, targetDataDir)
	}
	if resolvedTarget, err := filepath.EvalSymlinks(targetDataDir); err == nil {
		add(sourceDataDir, resolvedTarget)
		if meta != nil {
			add(meta.SnapshotSourceDataDir, resolvedTarget)
		}
		if resolvedSource, err := filepath.EvalSymlinks(sourceDataDir); err == nil {
			add(resolvedSource, resolvedTarget)
		}
		if meta != nil && meta.SnapshotSourceDataDir != "" {
			if resolvedSource, err := filepath.EvalSymlinks(meta.SnapshotSourceDataDir); err == nil {
				add(resolvedSource, resolvedTarget)
			}
		}
	}

	return rewrites
}
