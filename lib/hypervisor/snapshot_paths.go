package hypervisor

import "path/filepath"

// SnapshotDataRootDir returns the grandparent path for an instance data
// directory, used to rewrite snapshot paths that reference sibling instances.
func SnapshotDataRootDir(instanceDir string) string {
	clean := filepath.Clean(instanceDir)
	parent := filepath.Dir(clean)
	if parent == "." || parent == "/" || parent == clean {
		return ""
	}
	root := filepath.Dir(parent)
	if root == "." || root == "/" || root == parent {
		return ""
	}
	return root
}
