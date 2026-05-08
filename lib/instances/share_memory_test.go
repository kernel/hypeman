package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateForkRequest_ShareMemoryConflicts(t *testing.T) {
	t.Parallel()

	t.Run("share_memory with template_id is rejected", func(t *testing.T) {
		err := validateForkRequest(ForkInstanceRequest{
			Name:        "fork-bad-combo",
			ShareMemory: true,
			TemplateID:  "tpl-123",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("share_memory with from_running is rejected", func(t *testing.T) {
		err := validateForkRequest(ForkInstanceRequest{
			Name:        "fork-bad-combo",
			ShareMemory: true,
			FromRunning: true,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("share_memory alone is allowed", func(t *testing.T) {
		err := validateForkRequest(ForkInstanceRequest{
			Name:        "fork-ok",
			ShareMemory: true,
		})
		require.NoError(t, err)
	})
}

// stagedStandbySource creates a metadata + fake snapshot directory for an
// instance so toInstance reports State=Standby without involving any real
// hypervisor. Returns the source instance ID.
func stagedStandbySource(t *testing.T, mgr *manager, name string) string {
	t.Helper()
	id := name
	require.NoError(t, mgr.ensureDirectories(id))

	dataDir := mgr.paths.InstanceDir(id)
	snapDir := filepath.Join(dataDir, "snapshots", "snapshot-latest")
	require.NoError(t, os.MkdirAll(snapDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(snapDir, "memory"), []byte("fake-mem"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(snapDir, "config.json"), []byte("{}"), 0o644))

	now := time.Now()
	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                id,
		Name:              id,
		Image:             "docker.io/library/alpine:latest",
		CreatedAt:         now,
		HypervisorType:    hypervisor.TypeFirecracker,
		HypervisorVersion: "test",
		// Intentionally no SocketPath so deriveState falls through to the
		// snapshot check and reports Standby.
		DataDir:     dataDir,
		VsockCID:    44,
		VsockSocket: mgr.paths.InstanceVsockSocket(id),
	}}
	require.NoError(t, mgr.saveMetadata(meta))
	return id
}

func TestEnsureShareMemoryTemplate_AutoPromoteAndReuse(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := stagedStandbySource(t, mgr, "share-mem-source")

	tpl1, err := mgr.ensureShareMemoryTemplate(ctx, sourceID)
	require.NoError(t, err)
	require.NotNil(t, tpl1)
	assert.Equal(t, sourceID, tpl1.SourceInstanceID)
	assert.Equal(t, shareMemoryTemplateName(sourceID), tpl1.Name)

	// Source is now flagged as a template parent.
	meta, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	assert.True(t, meta.StoredMetadata.IsTemplate)
	assert.Equal(t, tpl1.ID, meta.StoredMetadata.TemplateID)

	// Second call returns the same registry entry — no duplicate promotion.
	tpl2, err := mgr.ensureShareMemoryTemplate(ctx, sourceID)
	require.NoError(t, err)
	assert.Equal(t, tpl1.ID, tpl2.ID)
}

func TestEnsureShareMemoryTemplate_RejectsNonStandby(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	// Same staged source layout but no snapshot dir → state derives as Stopped.
	id := "share-mem-stopped-source"
	require.NoError(t, mgr.ensureDirectories(id))
	now := time.Now()
	require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:                id,
		Name:              id,
		Image:             "docker.io/library/alpine:latest",
		CreatedAt:         now,
		HypervisorType:    hypervisor.TypeFirecracker,
		HypervisorVersion: "test",
		DataDir:           mgr.paths.InstanceDir(id),
		VsockCID:          45,
		VsockSocket:       mgr.paths.InstanceVsockSocket(id),
	}}))

	_, err := mgr.ensureShareMemoryTemplate(ctx, id)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)
}

func TestTemplateGuard_ReturnsInvalidStateNotUnsupported(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)

	// Locked: a template parent should return ErrInvalidState (409), not
	// ErrNotSupported (501) — the lock is transient (resolves once forks
	// are deleted), not a hypervisor capability gap.
	stored := &StoredMetadata{Id: "src", IsTemplate: true, TemplateID: "tpl-xyz"}
	err := mgr.templateGuard(stored, "start")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)
	assert.NotErrorIs(t, err, ErrNotSupported)

	// Not a template: no error.
	stored.IsTemplate = false
	require.NoError(t, mgr.templateGuard(stored, "start"))
}

func TestHydrateForkLockState(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := stagedStandbySource(t, mgr, "share-mem-hydrate-source")
	tpl, err := mgr.ensureShareMemoryTemplate(ctx, sourceID)
	require.NoError(t, err)

	// Zero forks initially.
	inst, err := mgr.GetInstance(ctx, sourceID)
	require.NoError(t, err)
	assert.Equal(t, 0, inst.ForkCount)
	assert.False(t, inst.MemLocked)

	// Bump refcount and re-read: ForkCount/MemLocked should reflect it.
	require.NoError(t, mgr.bumpTemplateForkRefcount(ctx, tpl))
	inst, err = mgr.GetInstance(ctx, sourceID)
	require.NoError(t, err)
	assert.Equal(t, 1, inst.ForkCount)
	assert.True(t, inst.MemLocked)
}
