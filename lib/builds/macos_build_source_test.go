package builds

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeMachineSource(t *testing.T, headers []*tar.Header, bodies []string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "source.tar.gz")
	f, e := os.Create(name)
	require.NoError(t, e)
	gz := gzip.NewWriter(f)
	writer := tar.NewWriter(gz)
	for i, h := range headers {
		require.NoError(t, writer.WriteHeader(h))
		if h.Typeflag == tar.TypeReg {
			_, e = writer.Write([]byte(bodies[i]))
			require.NoError(t, e)
		}
	}
	require.NoError(t, writer.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, f.Close())
	return name
}

func TestMachineSourceExtraction(t *testing.T) {
	source := writeMachineSource(t, []*tar.Header{{Name: machineEntrypoint, Size: 4, Typeflag: tar.TypeReg, Mode: 0755}, {Name: "lib/data", Size: 1, Typeflag: tar.TypeReg}}, []string{"true", "x"})
	root := filepath.Join(t.TempDir(), "source")
	require.NoError(t, extractMachineSource(context.Background(), source, root))
	info, e := os.Stat(filepath.Join(root, machineEntrypoint))
	require.NoError(t, e)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	info, e = os.Stat(filepath.Join(root, "lib", "data"))
	require.NoError(t, e)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestMachineSourceRejectsUnsafeArchives(t *testing.T) {
	entry := &tar.Header{Name: machineEntrypoint, Typeflag: tar.TypeReg, Size: 4}
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
		bodies  []string
	}{
		{"missing entrypoint", []*tar.Header{{Name: "other", Typeflag: tar.TypeReg, Size: 1}}, []string{"x"}},
		{"empty entrypoint", []*tar.Header{{Name: machineEntrypoint, Typeflag: tar.TypeReg}}, []string{""}},
		{"entrypoint directory", []*tar.Header{{Name: machineEntrypoint, Typeflag: tar.TypeDir}}, []string{""}},
		{"traversal", []*tar.Header{entry, {Name: "../outside", Typeflag: tar.TypeReg, Size: 1}}, []string{"true", "x"}},
		{"symlink", []*tar.Header{entry, {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}}, []string{"true", ""}},
		{"hardlink", []*tar.Header{entry, {Name: "link", Typeflag: tar.TypeLink, Linkname: "other"}}, []string{"true", ""}},
		{"duplicate", []*tar.Header{entry, {Name: "a", Typeflag: tar.TypeReg, Size: 1}, {Name: "a", Typeflag: tar.TypeReg, Size: 1}}, []string{"true", "x", "y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "source")
			require.Error(t, extractMachineSource(context.Background(), writeMachineSource(t, tc.headers, tc.bodies), root))
			require.NoDirExists(t, root)
		})
	}
}
