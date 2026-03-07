package snapshot

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildRecordAndDecodeRecordMetadata(t *testing.T) {
	t.Parallel()

	type stored struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	record, err := BuildRecord(Snapshot{Id: "snap-1", Name: "baseline"}, stored{
		ID:   "inst-1",
		Name: "vm",
	})
	require.NoError(t, err)
	require.Equal(t, "snap-1", record.Snapshot.Id)

	decoded, err := DecodeRecordMetadata[stored](record)
	require.NoError(t, err)
	require.Equal(t, "inst-1", decoded.ID)
	require.Equal(t, "vm", decoded.Name)
}
