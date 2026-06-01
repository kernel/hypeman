package instances

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/mailbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatchForkMailbox(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	memoryPath := filepath.Join(dir, firecrackerSnapshotMemoryFile)
	memory := make([]byte, 8192)
	marker, err := mailbox.ForkMailboxMarker("kernel.identity.v1", "template-token")
	require.NoError(t, err)
	const offset = 1024
	copy(memory[offset:], marker)
	require.NoError(t, os.WriteFile(memoryPath, memory, 0600))

	mgr := &manager{}
	stored := &StoredMetadata{Id: "forked-instance"}
	require.NoError(t, mgr.patchForkMailboxPayloads(context.Background(), stored, dir, []forkMailboxPatch{{
		name:    "kernel.identity.v1",
		token:   "template-token",
		payload: []byte(`{"instance_name":"forked"}`),
	}}))

	updated, err := os.ReadFile(memoryPath)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), binary.LittleEndian.Uint32(updated[offset+mailbox.ForkMailboxSeqOffset:]))
	payloadLen := binary.LittleEndian.Uint32(updated[offset+mailbox.ForkMailboxLengthOffset:])
	assert.Equal(t, uint32(len(`{"instance_name":"forked"}`)), payloadLen)
	assert.Equal(t, `{"instance_name":"forked"}`, string(updated[offset+mailbox.ForkMailboxPayloadOffset:offset+mailbox.ForkMailboxPayloadOffset+int(payloadLen)]))
}

func TestPatchForkMailboxPreflightsAllMarkers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	memoryPath := filepath.Join(dir, firecrackerSnapshotMemoryFile)
	memory := make([]byte, 8192)
	marker, err := mailbox.ForkMailboxMarker("kernel.identity.v1", "template-token")
	require.NoError(t, err)
	const offset = 1024
	copy(memory[offset:], marker)
	require.NoError(t, os.WriteFile(memoryPath, memory, 0600))

	mgr := &manager{}
	stored := &StoredMetadata{Id: "forked-instance"}
	err = mgr.patchForkMailboxPayloads(context.Background(), stored, dir, []forkMailboxPatch{
		{
			name:    "kernel.identity.v1",
			token:   "template-token",
			payload: []byte(`{"instance_name":"forked"}`),
		},
		{
			name:    "kernel.other.v1",
			token:   "other-token",
			payload: []byte(`{"value":true}`),
		},
	})
	require.Error(t, err)

	updated, err := os.ReadFile(memoryPath)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), binary.LittleEndian.Uint32(updated[offset+mailbox.ForkMailboxSeqOffset:]))
	assert.Equal(t, uint32(0), binary.LittleEndian.Uint32(updated[offset+mailbox.ForkMailboxLengthOffset:]))
}

func TestForkMailboxPayloadWithAckPort(t *testing.T) {
	t.Parallel()

	payload, err := forkMailboxPayloadWithAckPort([]byte(`{"instance_name":"forked"}`), 12345)
	require.NoError(t, err)
	assert.JSONEq(t, `{"instance_name":"forked","ack_port":12345}`, string(payload))
}

func TestValidateForkMailboxesRejectsPaddedName(t *testing.T) {
	t.Parallel()

	err := validateForkMailboxes([]ForkMailboxPayload{{
		Name:    " kernel.identity.v1 ",
		Token:   "template-token",
		Payload: []byte(`{"instance_name":"forked"}`),
	}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
}

func TestValidateForkMailboxHypervisor(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateForkMailboxHypervisor(hypervisor.TypeFirecracker))

	err := validateForkMailboxHypervisor(hypervisor.TypeCloudHypervisor)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotSupported)
}
