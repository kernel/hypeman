package templates

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRegistry(t *testing.T) *FileRegistry {
	t.Helper()
	return NewFileRegistry(filepath.Join(t.TempDir(), "templates"))
}

func sampleTemplate(id, name string) *Template {
	return &Template{
		ID:                id,
		Name:              name,
		SourceInstanceID:  "src-" + id,
		Image:             "docker.io/library/alpine:latest",
		ImageDigest:       "sha256:deadbeef",
		HypervisorType:    hypervisor.TypeFirecracker,
		HypervisorVersion: "v1.14.2",
		MemoryBytes:       1 << 30,
		VCPUs:             2,
		CreatedAt:         time.Now().UTC(),
	}
}

func TestFileRegistry_SaveGet(t *testing.T) {
	r := newTestRegistry(t)
	tpl := sampleTemplate("t1", "alpine-warm")

	require.NoError(t, r.Save(context.Background(), tpl))

	got, err := r.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, "alpine-warm", got.Name)
	assert.Equal(t, hypervisor.TypeFirecracker, got.HypervisorType)
}

func TestFileRegistry_GetByName(t *testing.T) {
	r := newTestRegistry(t)
	require.NoError(t, r.Save(context.Background(), sampleTemplate("t1", "alpha")))
	require.NoError(t, r.Save(context.Background(), sampleTemplate("t2", "beta")))

	got, err := r.GetByName(context.Background(), "beta")
	require.NoError(t, err)
	assert.Equal(t, "t2", got.ID)

	_, err = r.GetByName(context.Background(), "missing")
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestFileRegistry_List_Filter(t *testing.T) {
	r := newTestRegistry(t)
	a := sampleTemplate("a", "a")
	b := sampleTemplate("b", "b")
	b.ImageDigest = "sha256:other"
	c := sampleTemplate("c", "c")
	c.HypervisorType = hypervisor.TypeCloudHypervisor

	require.NoError(t, r.Save(context.Background(), a))
	require.NoError(t, r.Save(context.Background(), b))
	require.NoError(t, r.Save(context.Background(), c))

	all, err := r.List(context.Background(), nil)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	byHV, err := r.List(context.Background(), &ListFilter{HypervisorType: hypervisor.TypeFirecracker})
	require.NoError(t, err)
	assert.Len(t, byHV, 2)

	byDigest, err := r.List(context.Background(), &ListFilter{ImageDigest: "sha256:deadbeef"})
	require.NoError(t, err)
	assert.Len(t, byDigest, 2)
}

func TestFileRegistry_Refcount(t *testing.T) {
	r := newTestRegistry(t)
	require.NoError(t, r.Save(context.Background(), sampleTemplate("t1", "a")))

	got, err := r.IncrementForkCount(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, 1, got.ForkCount)

	got, err = r.IncrementForkCount(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, 2, got.ForkCount)

	err = r.Delete(context.Background(), "t1")
	assert.True(t, errors.Is(err, ErrInUse))

	got, err = r.DecrementForkCount(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, 1, got.ForkCount)
	got, err = r.DecrementForkCount(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, 0, got.ForkCount)

	err = r.Delete(context.Background(), "t1")
	require.NoError(t, err)

	_, err = r.Get(context.Background(), "t1")
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestFileRegistry_DecrementMissingIsNoop(t *testing.T) {
	r := newTestRegistry(t)
	got, err := r.DecrementForkCount(context.Background(), "missing")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestFileRegistry_SaveValidates(t *testing.T) {
	r := newTestRegistry(t)
	err := r.Save(context.Background(), &Template{Name: "x"})
	assert.True(t, errors.Is(err, ErrInvalid))
}

func TestFileRegistry_Reconcile(t *testing.T) {
	r := newTestRegistry(t)
	a := sampleTemplate("a", "alpha")
	a.ForkCount = 5
	b := sampleTemplate("b", "beta")
	b.ForkCount = 0
	c := sampleTemplate("c", "gamma")
	c.ForkCount = 7
	require.NoError(t, r.Save(context.Background(), a))
	require.NoError(t, r.Save(context.Background(), b))
	require.NoError(t, r.Save(context.Background(), c))

	require.NoError(t, r.Reconcile(context.Background(), map[string]int{
		"a": 2,
		"b": 3,
		// c omitted -> should fall to 0
	}))

	got, err := r.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.Equal(t, 2, got.ForkCount)
	got, err = r.Get(context.Background(), "b")
	require.NoError(t, err)
	assert.Equal(t, 3, got.ForkCount)
	got, err = r.Get(context.Background(), "c")
	require.NoError(t, err)
	assert.Equal(t, 0, got.ForkCount)
}
