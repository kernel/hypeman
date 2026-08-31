// Package testutil provides helpers for seeding on-disk image state in tests.
package testutil

import (
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// Seed describes a ready image to seed; see images.TestSeed for the fields.
type Seed = images.TestSeed

// SeedReadyImage writes a ready image to disk per s.
func SeedReadyImage(t testing.TB, p *paths.Paths, s Seed) {
	t.Helper()
	require.NoError(t, images.SeedTestImage(p, s))
}
