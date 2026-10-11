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

func TestMachineRecipeExtraction(t *testing.T) {
	recipe := `{"version":1,"steps":[{"command":["/bin/sh","build.sh"],"env":{"MODE":"qa"}}]}`
	source := writeMachineSource(t, []*tar.Header{{Name: machineRecipeName, Size: int64(len(recipe)), Typeflag: tar.TypeReg}, {Name: "build.sh", Size: 4, Typeflag: tar.TypeReg, Mode: 0755}}, []string{recipe, "true"})
	root := filepath.Join(t.TempDir(), "source")
	result, e := extractMachineRecipe(context.Background(), source, root)
	require.NoError(t, e)
	require.Len(t, result.Steps, 1)
	info, e := os.Stat(filepath.Join(root, "build.sh"))
	require.NoError(t, e)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
}

func TestMachineRecipeRejectsUnsafeArchivesAndCommands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
		bodies  []string
	}{
		{"traversal", []*tar.Header{{Name: "../outside", Typeflag: tar.TypeReg, Size: 1}}, []string{"x"}},
		{"symlink", []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}}, []string{""}},
		{"hardlink", []*tar.Header{{Name: "link", Typeflag: tar.TypeLink, Linkname: "other"}}, []string{""}},
		{"duplicate", []*tar.Header{{Name: "a", Typeflag: tar.TypeReg, Size: 1}, {Name: "a", Typeflag: tar.TypeReg, Size: 1}}, []string{"x", "y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "source")
			_, e := extractMachineRecipe(context.Background(), writeMachineSource(t, tc.headers, tc.bodies), root)
			require.Error(t, e)
			_, e = os.Stat(root)
			require.True(t, os.IsNotExist(e))
		})
	}
	for _, body := range []string{`{"version":2,"steps":[]}`, `{"version":1,"steps":[{"command":["sh"]}]}`, `{"version":1,"steps":[{"command":["/bin/true"],"secret":"no"}]}`, `{"version":1,"steps":[{"command":["/bin/true"]}]} {}`} {
		source := writeMachineSource(t, []*tar.Header{{Name: machineRecipeName, Typeflag: tar.TypeReg, Size: int64(len(body))}}, []string{body})
		_, e := extractMachineRecipe(context.Background(), source, filepath.Join(t.TempDir(), "source"))
		require.Error(t, e)
	}
}
