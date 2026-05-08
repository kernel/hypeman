package forkidentity

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuild_FreshSeedPerCall(t *testing.T) {
	a, err := Build("fork-1")
	require.NoError(t, err)
	b, err := Build("fork-1")
	require.NoError(t, err)
	assert.False(t, bytes.Equal(a.EntropySeed, b.EntropySeed),
		"two builds must not share entropy seeds")
}

func TestBuild_RejectsEmptyForkID(t *testing.T) {
	_, err := Build("")
	assert.Error(t, err)
}

func TestBuild_PopulatesAllFields(t *testing.T) {
	id, err := Build("fork-7")
	require.NoError(t, err)

	assert.Equal(t, CurrentVersion, id.Version)
	assert.Equal(t, "fork-7", id.ForkID)
	assert.Len(t, id.EntropySeed, EntropySeedBytes)
	assert.False(t, id.CreatedAt.IsZero())
	assert.GreaterOrEqual(t, id.ClockOffsetNs, int64(0))
}

func TestWriteRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want, err := Build("fork-rt")
	require.NoError(t, err)
	require.NoError(t, Write(dir, want))

	got, err := Read(dir)
	require.NoError(t, err)
	assert.Equal(t, want.ForkID, got.ForkID)
	assert.Equal(t, want.Version, got.Version)
	assert.Equal(t, want.ClockOffsetNs, got.ClockOffsetNs)
	assert.True(t, bytes.Equal(want.EntropySeed, got.EntropySeed))
}

func TestRead_MissingIsErrEmpty(t *testing.T) {
	_, err := Read(t.TempDir())
	assert.True(t, errors.Is(err, ErrEmpty))
}

func TestWrite_RejectsZeroVersion(t *testing.T) {
	err := Write(t.TempDir(), Identity{ForkID: "x"})
	assert.Error(t, err)
}
