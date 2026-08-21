package images

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireQEMUImg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		if os.Getenv("CI") == "true" && runtime.GOOS == "linux" {
			t.Fatal("qemu-img is required in Linux CI")
		}
		t.Skip("qemu-img is unavailable")
	}
}

func windowsMachineMetadata(kind MachineImageKind, diskPath, base string) *containerMetadata {
	format := "raw"
	if kind == MachineImageWindowsImage {
		format = "qcow2"
	}
	return &containerMetadata{
		OS:           "windows",
		Architecture: "amd64",
		Labels: map[string]string{
			MachineImageVersionLabel:    MachineImageVersion,
			MachineImageKindLabel:       string(kind),
			MachineImageDiskPathLabel:   diskPath,
			MachineImageDiskFormatLabel: format,
			MachineImageBaseLabel:       base,
			MachineImageTPMLabel:        "2.0",
			MachineImageSecureBootLabel: "required",
		},
	}
}

func TestParseMachineImage(t *testing.T) {
	base, err := parseMachineImage(windowsMachineMetadata(MachineImageWindowsBase, "hypeman/disk.raw", ""))
	require.NoError(t, err)
	assert.Equal(t, MachineImageWindowsBase, base.Kind)

	image, err := parseMachineImage(windowsMachineMetadata(
		MachineImageWindowsImage,
		"hypeman/disk.qcow2",
		"registry.example/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	))
	require.NoError(t, err)
	assert.Equal(t, MachineImageWindowsImage, image.Kind)

	_, err = parseMachineImage(&containerMetadata{OS: "windows", Architecture: "amd64", Labels: map[string]string{}})
	assert.ErrorContains(t, err, "ordinary Windows container images are not bootable")

	invalidPath := windowsMachineMetadata(MachineImageWindowsBase, "../disk.raw", "")
	_, err = parseMachineImage(invalidPath)
	assert.ErrorContains(t, err, "local relative path")
}

func TestMachineArtifactDiskRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "disk.raw")
	require.NoError(t, os.WriteFile(outside, []byte("disk"), 0644))
	require.NoError(t, os.Symlink(filepath.Dir(outside), filepath.Join(root, "hypeman")))

	_, err := machineArtifactDisk(root, &MachineImage{DiskPath: "hypeman/disk.raw"})
	assert.ErrorContains(t, err, "escapes artifact root")
}

func TestMaterializeRejectsExternalDiskReferences(t *testing.T) {
	requireQEMUImg(t)

	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "hypeman"), 0755))

	backed := filepath.Join(root, "hypeman", "backed.qcow2")
	output, err := exec.Command("qemu-img", "create", "-f", "qcow2", "-F", "raw", "-b", "/etc/passwd", backed, "4M").CombinedOutput()
	require.NoError(t, err, "%s", output)
	meta := windowsMachineMetadata(MachineImageWindowsBase, "hypeman/backed.qcow2", "")
	meta.Labels[MachineImageDiskFormatLabel] = "qcow2"
	machine, err := parseMachineImage(meta)
	require.NoError(t, err)
	ref, err := ParseNormalizedRef("registry.example/windows/base@sha256:" + strings.Repeat("5", 64))
	require.NoError(t, err)
	_, err = m.materializeMachineImage(NewResolvedRef(ref, ref.Digest()), root, machine)
	assert.ErrorContains(t, err, "must not reference a backing file")

	dataFile := filepath.Join(t.TempDir(), "external.raw")
	require.NoError(t, os.WriteFile(dataFile, make([]byte, 4<<20), 0644))
	external := filepath.Join(root, "hypeman", "external.qcow2")
	output, err = exec.Command("qemu-img", "create", "-f", "qcow2", "-o", "data_file="+dataFile+",data_file_raw=on", external, "4M").CombinedOutput()
	require.NoError(t, err, "%s", output)
	image := windowsMachineMetadata(
		MachineImageWindowsImage,
		"hypeman/external.qcow2",
		"registry.example/windows/base@sha256:"+strings.Repeat("6", 64),
	)
	machine, err = parseMachineImage(image)
	require.NoError(t, err)
	imageRef, err := ParseNormalizedRef("registry.example/windows/image@sha256:" + strings.Repeat("7", 64))
	require.NoError(t, err)
	_, err = m.materializeMachineImage(NewResolvedRef(imageRef, imageRef.Digest()), root, machine)
	assert.ErrorContains(t, err, "unsupported data-file feature")
}

func TestMaterializeWindowsBaseFormats(t *testing.T) {
	requireQEMUImg(t)

	formats := []struct {
		label string
		qemu  string
		char  string
	}{
		{label: "raw", qemu: "raw", char: "1"},
		{label: "qcow2", qemu: "qcow2", char: "2"},
		{label: "vhd", qemu: "vpc", char: "3"},
		{label: "vhdx", qemu: "vhdx", char: "4"},
	}
	for _, format := range formats {
		t.Run(format.label, func(t *testing.T) {
			p := paths.New(t.TempDir())
			m := &manager{paths: p}
			digest := strings.Repeat(format.char, 64)
			ref, err := ParseNormalizedRef("registry.example/windows/base@sha256:" + digest)
			require.NoError(t, err)
			resolved := NewResolvedRef(ref, "sha256:"+digest)
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "hypeman"), 0755))
			source := filepath.Join(root, "hypeman", "disk")
			output, err := exec.Command("qemu-img", "create", "-f", format.qemu, source, "4M").CombinedOutput()
			require.NoError(t, err, "%s", output)

			meta := windowsMachineMetadata(MachineImageWindowsBase, "hypeman/disk", "")
			meta.Labels[MachineImageDiskFormatLabel] = format.label
			machine, err := parseMachineImage(meta)
			require.NoError(t, err)
			_, err = m.materializeMachineImage(resolved, root, machine)
			require.NoError(t, err)
			info, err := inspectQEMUImage(machineDiskPath(p, ref.Repository(), digest, MachineImageWindowsBase), "raw")
			require.NoError(t, err)
			assert.Equal(t, "raw", info.Format)
		})
	}
}

func TestPendingWindowsImageBlocksBaseDeletion(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	baseDigest := strings.Repeat("a", 64)
	imageDigest := strings.Repeat("b", 64)
	baseName := "registry.example/windows/base@sha256:" + baseDigest
	imageName := "registry.example/windows/image@sha256:" + imageDigest

	require.NoError(t, writeMetadata(p, "registry.example/windows/base", baseDigest, &imageMetadata{
		Name:     baseName,
		Digest:   "sha256:" + baseDigest,
		Platform: "windows/amd64",
		Status:   StatusReady,
		Machine:  &MachineImage{Kind: MachineImageWindowsBase},
	}))
	require.NoError(t, os.WriteFile(machineDiskPath(p, "registry.example/windows/base", baseDigest, MachineImageWindowsBase), []byte("base"), 0444))
	const buildID = "pending-build"
	require.NoError(t, writeMetadata(p, "registry.example/windows/image", imageDigest, &imageMetadata{
		Name:     imageName,
		Digest:   "sha256:" + imageDigest,
		Platform: "windows/amd64",
		Status:   StatusPending,
		BuildID:  buildID,
	}))
	ref, err := ParseNormalizedRef(imageName)
	require.NoError(t, err)
	require.NoError(t, m.recordMachineDependency(NewResolvedRef(ref, ref.Digest()), &MachineImage{
		Kind: MachineImageWindowsImage,
		Base: baseName,
	}, buildID))

	err = m.DeleteImage(t.Context(), baseName)
	assert.ErrorContains(t, err, "depends on it")
	assert.DirExists(t, p.ImageDigestDir("registry.example/windows/base", baseDigest))
}

func TestMaterializeWindowsBaseAndImage(t *testing.T) {
	requireQEMUImg(t)

	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	baseDigest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseRef, err := ParseNormalizedRef("registry.example/windows/base@sha256:" + baseDigest)
	require.NoError(t, err)
	resolvedBase := NewResolvedRef(baseRef, "sha256:"+baseDigest)

	baseRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(baseRoot, "hypeman"), 0755))
	baseSource := filepath.Join(baseRoot, "hypeman", "disk.raw")
	baseFile, err := os.Create(baseSource)
	require.NoError(t, err)
	require.NoError(t, baseFile.Truncate(4<<20))
	require.NoError(t, baseFile.Close())

	baseMachine, err := parseMachineImage(windowsMachineMetadata(MachineImageWindowsBase, "hypeman/disk.raw", ""))
	require.NoError(t, err)
	_, err = m.materializeMachineImage(resolvedBase, baseRoot, baseMachine)
	require.NoError(t, err)
	require.NoError(t, writeMetadata(p, baseRef.Repository(), baseDigest, &imageMetadata{
		Name:      baseRef.String(),
		Digest:    "sha256:" + baseDigest,
		Platform:  "windows/amd64",
		Status:    StatusReady,
		Machine:   baseMachine,
		SizeBytes: 4 << 20,
	}))

	imageDigest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	imageRef, err := ParseNormalizedRef("registry.example/windows/image@sha256:" + imageDigest)
	require.NoError(t, err)
	resolvedImage := NewResolvedRef(imageRef, "sha256:"+imageDigest)
	imageRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(imageRoot, "hypeman"), 0755))
	imageSource := filepath.Join(imageRoot, "hypeman", "disk.qcow2")
	output, err := exec.Command("qemu-img", "create", "-f", "qcow2", imageSource, "4M").CombinedOutput()
	require.NoError(t, err, "%s", output)

	imageMachine, err := parseMachineImage(windowsMachineMetadata(
		MachineImageWindowsImage,
		"hypeman/disk.qcow2",
		baseRef.String(),
	))
	require.NoError(t, err)
	_, err = m.materializeMachineImage(resolvedImage, imageRoot, imageMachine)
	require.NoError(t, err)

	imagePath := machineDiskPath(p, imageRef.Repository(), imageDigest, MachineImageWindowsImage)
	info, err := inspectQEMUImage(imagePath, "qcow2")
	require.NoError(t, err)
	assert.Equal(t, "qcow2", info.Format)
	assert.Equal(t, "raw", info.BackingFileFormat)
	assert.Equal(t, machineDiskPath(p, baseRef.Repository(), baseDigest, MachineImageWindowsBase), info.BackingFilename)

	baseMeta, err := readMetadata(p, baseRef.Repository(), baseDigest)
	require.NoError(t, err)
	imageMeta := &imageMetadata{
		Name:      imageRef.String(),
		Digest:    "sha256:" + imageDigest,
		Platform:  "windows/amd64",
		Status:    StatusReady,
		Machine:   imageMachine,
		SizeBytes: info.VirtualSize,
	}
	require.NoError(t, writeMetadata(p, imageRef.Repository(), imageDigest, imageMeta))
	assert.ErrorContains(t, m.ensureNoMachineDependents(baseRef.Repository(), baseDigest), "depends on it")
	assert.ErrorContains(t, m.DeleteImage(t.Context(), baseRef.String()), "depends on it")
	assert.DirExists(t, p.ImageDigestDir(baseRef.Repository(), baseDigest))
	assert.Equal(t, MachineImageWindowsBase, baseMeta.toImage().Machine.Kind)
}
