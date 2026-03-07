package snapshot

import (
	"encoding/json"
	"fmt"
)

// TypedRecord is a strongly-typed snapshot record for callers that want
// structured stored metadata instead of json.RawMessage.
type TypedRecord[T any] struct {
	Snapshot       Snapshot
	StoredMetadata T
}

func SaveTypedRecord[T any](store *Store, record *TypedRecord[T]) error {
	if record == nil {
		return fmt.Errorf("nil snapshot record")
	}
	encoded, err := encodeTypedRecord(record)
	if err != nil {
		return err
	}
	return store.SaveRecord(encoded)
}

func LoadTypedRecord[T any](store *Store, snapshotID string) (*TypedRecord[T], error) {
	record, err := store.LoadRecord(snapshotID)
	if err != nil {
		return nil, err
	}
	return decodeTypedRecord[T](record)
}

func ListTypedRecords[T any](store *Store) ([]TypedRecord[T], error) {
	records, err := store.ListRecords()
	if err != nil {
		return nil, err
	}
	typed := make([]TypedRecord[T], 0, len(records))
	for i := range records {
		decoded, err := decodeTypedRecord[T](&records[i])
		if err != nil {
			return nil, err
		}
		typed = append(typed, *decoded)
	}
	return typed, nil
}

func encodeTypedRecord[T any](record *TypedRecord[T]) (*Record, error) {
	metadata, err := json.Marshal(record.StoredMetadata)
	if err != nil {
		return nil, fmt.Errorf("marshal stored metadata: %w", err)
	}
	return &Record{
		Snapshot:       record.Snapshot,
		StoredMetadata: metadata,
	}, nil
}

func decodeTypedRecord[T any](record *Record) (*TypedRecord[T], error) {
	var metadata T
	if err := json.Unmarshal(record.StoredMetadata, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshal stored metadata for snapshot %s: %w", record.Snapshot.Id, err)
	}
	return &TypedRecord[T]{
		Snapshot:       record.Snapshot,
		StoredMetadata: metadata,
	}, nil
}
