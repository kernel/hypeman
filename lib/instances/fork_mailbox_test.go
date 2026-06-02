package instances

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestWaitMailboxAppliedRequiresExactFields(t *testing.T) {
	t.Parallel()

	waiter := &guestResumeNetworkUDPWaiter{ch: make(chan guestResumeNetworkUDPAck, 2)}
	now := time.Now()
	waiter.ch <- guestResumeNetworkUDPAck{
		received: now,
		text:     "stage=applied mailbox=kernel.identity.v10",
	}
	waiter.ch <- guestResumeNetworkUDPAck{
		received: now.Add(time.Millisecond),
		text:     "mailbox=kernel.identity.v1 stage=applied",
	}

	_, ack, err := waiter.WaitMailboxApplied(context.Background(), "kernel.identity.v1")
	require.NoError(t, err)
	assert.Equal(t, "mailbox=kernel.identity.v1 stage=applied", ack)
}

func TestWaitMailboxAppliedIgnoresMalformedAck(t *testing.T) {
	t.Parallel()

	waiter := &guestResumeNetworkUDPWaiter{ch: make(chan guestResumeNetworkUDPAck, 1)}
	waiter.ch <- guestResumeNetworkUDPAck{
		received: time.Now(),
		text:     "stage=applied mailbox=kernel.identity.v1-extra freeform",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, _, err := waiter.WaitMailboxApplied(ctx, "kernel.identity.v1")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWaitAppliedRequiresExactFields(t *testing.T) {
	t.Parallel()

	waiter := &guestResumeNetworkUDPWaiter{ch: make(chan guestResumeNetworkUDPAck, 2)}
	now := time.Now()
	waiter.ch <- guestResumeNetworkUDPAck{
		received: now,
		text:     "stage=applied mac=02:00:00:85:17:c8:ff ip=10.102.146.62",
	}
	waiter.ch <- guestResumeNetworkUDPAck{
		received: now.Add(time.Millisecond),
		text:     "ip=10.102.146.62 stage=applied mac=02:00:00:85:17:C8",
	}

	_, ack, err := waiter.WaitApplied(context.Background(), "02:00:00:85:17:c8", "10.102.146.62")
	require.NoError(t, err)
	assert.Equal(t, "ip=10.102.146.62 stage=applied mac=02:00:00:85:17:C8", ack)
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

func TestValidateForkMailboxesAckTimeout(t *testing.T) {
	t.Parallel()

	base := ForkMailboxPayload{
		Name:    "kernel.identity.v1",
		Token:   "template-token",
		Payload: []byte(`{"instance_name":"forked"}`),
	}

	withDefaultTimeout := base
	withDefaultTimeout.WaitForAck = true
	require.NoError(t, validateForkMailboxes([]ForkMailboxPayload{withDefaultTimeout}))

	withNegativeTimeout := base
	withNegativeTimeout.WaitForAck = true
	withNegativeTimeout.AckTimeout = -time.Millisecond
	err := validateForkMailboxes([]ForkMailboxPayload{withNegativeTimeout})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)

	ignoredWithoutAck := base
	ignoredWithoutAck.AckTimeout = 31 * time.Second
	require.NoError(t, validateForkMailboxes([]ForkMailboxPayload{ignoredWithoutAck}))
}

func TestValidateForkMailboxHypervisor(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateForkMailboxHypervisor(hypervisor.TypeFirecracker))

	err := validateForkMailboxHypervisor(hypervisor.TypeCloudHypervisor)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotSupported)
}
