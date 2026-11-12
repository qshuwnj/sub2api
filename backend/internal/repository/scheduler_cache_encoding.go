package repository

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

const (
	schedulerCacheCompressionThreshold = 64 << 10
	schedulerCacheMaxExpandedBytes     = 64 << 20
)

// Large group tariffs are repeated in full account snapshots. Compress the
// opaque JSON instead of dropping fields that a scheduler consumer may need.
func encodeSchedulerAccountPayload(payload []byte) ([]byte, error) {
	if len(payload) < schedulerCacheCompressionThreshold || len(payload) > schedulerCacheMaxExpandedBytes {
		return payload, nil
	}
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(payload); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() >= len(payload) {
		return payload, nil
	}
	return compressed.Bytes(), nil
}

func expandSchedulerAccountPayload(payload []byte) ([]byte, error) {
	if len(payload) < 2 || payload[0] != 0x1f || payload[1] != 0x8b {
		return payload, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("open compressed scheduler account: %w", err)
	}
	defer reader.Close()
	expanded, err := io.ReadAll(io.LimitReader(reader, schedulerCacheMaxExpandedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("expand scheduler account: %w", err)
	}
	if len(expanded) > schedulerCacheMaxExpandedBytes {
		return nil, fmt.Errorf("compressed scheduler account exceeds expansion limit")
	}
	return expanded, nil
}
