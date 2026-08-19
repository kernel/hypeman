package images

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func windowsMachineMetadata(kind MachineImageKind, diskPath, base string) *containerMetadata {
	format := "raw"
	if kind == MachineImageWindowsPersona {
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

	persona, err := parseMachineImage(windowsMachineMetadata(
		MachineImageWindowsPersona,
		"hypeman/disk.qcow2",
		"registry.example/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	))
	require.NoError(t, err)
	assert.Equal(t, MachineImageWindowsPersona, persona.Kind)

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

func TestMaterializeWindowsBaseFormats(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("qemu-img is required in CI")
		}
		t.Skip("qemu-img is unavailable")
	}

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
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "hypeman"), 0755))
			source := filepath.Join(root, "hypeman", "disk")
			output, err := exec.Command("qemu-img", "create", "-f", format.qemu, source, "4M").CombinedOutput()
			require.NoError(t, err, "%s", output)

			meta := windowsMachineMetadata(MachineImageWindowsBase, "hypeman/disk", "")
			meta.Labels[MachineImageDiskFormatLabel] = format.label
			machine, err := parseMachineImage(meta)
			require.NoError(t, err)
			_, err = m.materializeMachineImage(root, machine, machineDiskPath(p, ref.Repository(), digest, machine.Kind))
			require.NoError(t, err)
			info, err := inspectQEMUImage(machineDiskPath(p, ref.Repository(), digest, MachineImageWindowsBase))
			require.NoError(t, err)
			assert.Equal(t, "raw", info.Format)
		})
	}
}

func TestMaterializeWindowsBaseAndPersona(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("qemu-img is required in CI")
		}
		t.Skip("qemu-img is unavailable")
	}

	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	baseDigest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseRef, err := ParseNormalizedRef("registry.example/windows/base@sha256:" + baseDigest)
	require.NoError(t, err)
	baseRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(baseRoot, "hypeman"), 0755))
	baseSource := filepath.Join(baseRoot, "hypeman", "disk.raw")
	baseFile, err := os.Create(baseSource)
	require.NoError(t, err)
	require.NoError(t, baseFile.Truncate(4<<20))
	require.NoError(t, baseFile.Close())

	baseMachine, err := parseMachineImage(windowsMachineMetadata(MachineImageWindowsBase, "hypeman/disk.raw", ""))
	require.NoError(t, err)
	_, err = m.materializeMachineImage(baseRoot, baseMachine, machineDiskPath(p, baseRef.Repository(), baseDigest, baseMachine.Kind))
	require.NoError(t, err)
	require.NoError(t, writeMetadata(p, baseRef.Repository(), baseDigest, &imageMetadata{
		Name:      baseRef.String(),
		Digest:    "sha256:" + baseDigest,
		Platform:  "windows/amd64",
		Status:    StatusReady,
		Machine:   baseMachine,
		SizeBytes: 4 << 20,
	}))

	personaDigest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	personaRef, err := ParseNormalizedRef("registry.example/windows/persona@sha256:" + personaDigest)
	require.NoError(t, err)
	personaRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(personaRoot, "hypeman"), 0755))
	personaSource := filepath.Join(personaRoot, "hypeman", "disk.qcow2")
	output, err := exec.Command("qemu-img", "create", "-f", "qcow2", personaSource, "4M").CombinedOutput()
	require.NoError(t, err, "%s", output)

	personaMachine, err := parseMachineImage(windowsMachineMetadata(
		MachineImageWindowsPersona,
		"hypeman/disk.qcow2",
		baseRef.String(),
	))
	require.NoError(t, err)
	_, err = m.materializeMachineImage(personaRoot, personaMachine, machineDiskPath(p, personaRef.Repository(), personaDigest, personaMachine.Kind))
	require.NoError(t, err)

	personaPath := machineDiskPath(p, personaRef.Repository(), personaDigest, MachineImageWindowsPersona)
	info, err := inspectQEMUImage(personaPath)
	require.NoError(t, err)
	assert.Equal(t, "qcow2", info.Format)
	assert.Equal(t, "raw", info.BackingFileFormat)
	assert.Equal(t, machineDiskPath(p, baseRef.Repository(), baseDigest, MachineImageWindowsBase), info.BackingFilename)

	baseMeta, err := readMetadata(p, baseRef.Repository(), baseDigest)
	require.NoError(t, err)
	personaMeta := &imageMetadata{
		Name:      personaRef.String(),
		Digest:    "sha256:" + personaDigest,
		Platform:  "windows/amd64",
		Status:    StatusReady,
		Machine:   personaMachine,
		SizeBytes: info.VirtualSize,
	}
	require.NoError(t, writeMetadata(p, personaRef.Repository(), personaDigest, personaMeta))
	assert.ErrorContains(t, m.ensureNoMachineDependents(baseRef.Repository(), baseDigest), "depends on it")
	assert.ErrorContains(t, m.DeleteImage(t.Context(), baseRef.String()), "depends on it")
	assert.DirExists(t, p.ImageDigestDir(baseRef.Repository(), baseDigest))
	assert.Equal(t, MachineImageWindowsBase, baseMeta.toImage().Machine.Kind)
}
