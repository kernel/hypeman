package instances

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newMetadataScanTestManager(t *testing.T, ids ...string) (*manager, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	m := &manager{paths: paths.New(t.TempDir()), tracer: provider.Tracer("test")}
	for _, id := range ids {
		require.NoError(t, m.ensureDirectories(id))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
			Id:        id,
			Name:      "name-" + id,
			CreatedAt: time.Now(),
			DataDir:   m.paths.InstanceDir(id),
		}}))
	}
	return m, recorder
}

func metadataScanSpanAttrs(t *testing.T, recorder *tracetest.SpanRecorder, name string) map[attribute.Key]attribute.Value {
	t.Helper()
	for _, span := range recorder.Ended() {
		if span.Name() != name {
			continue
		}
		attrs := make(map[attribute.Key]attribute.Value)
		for _, kv := range span.Attributes() {
			attrs[kv.Key] = kv.Value
		}
		return attrs
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func TestFindExactNameAttributesAliasLockWait(t *testing.T) {
	m, recorder := newMetadataScanTestManager(t, "aaa", "bbb")

	unlock := hypervisor.LockSnapshotSourceAliasMutation()
	done := make(chan error, 1)
	go func() {
		_, err := m.findInstanceMetadataByExactName(context.Background(), "name-bbb")
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	unlock()
	require.NoError(t, <-done)

	attrs := metadataScanSpanAttrs(t, recorder, "instances.metadata.find_exact_name")
	assert.GreaterOrEqual(t, attrs["alias_lock_wait_ms"].AsInt64(), int64(50))
	assert.Less(t, attrs["read_ms"].AsInt64(), int64(50))
	assert.Equal(t, "alias_lock_wait", attrs["slowest_step"].AsString())
	assert.GreaterOrEqual(t, attrs["slowest_step_ms"].AsInt64(), int64(50))
	assert.Equal(t, "aaa", attrs["slowest_step_instance_id"].AsString())
	assert.NotContains(t, attrs, attribute.Key("hydrate_ms"))
}

func TestMetadataScansRecordStepTimings(t *testing.T) {
	m, recorder := newMetadataScanTestManager(t, "aaa", "bbb")
	ctx := context.Background()

	_, err := m.ListInstances(ctx, nil)
	require.NoError(t, err)
	_, err = m.findInstanceMetadataByExactName(ctx, "name-bbb")
	require.NoError(t, err)
	_, err = m.findInstanceMetadataByNameOrIDPrefix(ctx, "name-bbb", 1)
	require.NoError(t, err)

	for _, name := range []string{
		"instances.list_metadata",
		"instances.metadata.find_exact_name",
		"instances.metadata.find_name_or_id_prefix",
	} {
		attrs := metadataScanSpanAttrs(t, recorder, name)
		for _, key := range []attribute.Key{"list_dir_ms", "stat_ms", "alias_lock_wait_ms", "read_ms", "decode_ms", "slowest_step"} {
			assert.Contains(t, attrs, key, "span %s", name)
		}
	}
	assert.Contains(t, metadataScanSpanAttrs(t, recorder, "instances.list_metadata"), attribute.Key("hydrate_ms"))
}
