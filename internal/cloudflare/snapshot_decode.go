package cloudflare

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// MaxSnapshotBytes bounds consumer-supplied fallback artifacts before decoding.
const MaxSnapshotBytes = 256 << 10

// DecodeSnapshot accepts the exact generated artifact schema and returns an
// independently owned, order-normalized fallback snapshot.
func DecodeSnapshot(data []byte) (Snapshot, error) {
	if len(data) == 0 || len(data) > MaxSnapshotBytes {
		return Snapshot{}, fmt.Errorf("snapshot must contain 1..%d bytes", MaxSnapshotBytes)
	}
	if err := validateSnapshotJSON(data); err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: %w", err)
	}
	return snapshot.Normalized(), nil
}

// Normalized copies and sorts both families and normalizes timestamp location.
// Callers validate external snapshots before using this representation.
func (s Snapshot) Normalized() Snapshot {
	s.IPv4 = slices.Clone(s.IPv4)
	s.IPv6 = slices.Clone(s.IPv6)
	slices.Sort(s.IPv4)
	slices.Sort(s.IPv6)
	s.FetchedAt = s.FetchedAt.UTC()
	return s
}

// validateSnapshotJSON rejects ambiguous duplicate and case-aliased members.
func validateSnapshotJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errors.New("snapshot must be a JSON object")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		if err := readSnapshotMember(decoder, seen); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("snapshot closing object: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("snapshot contains trailing JSON data")
	}
	if len(seen) != 3 {
		return errors.New("snapshot requires ipv4, ipv6 and fetched_at")
	}
	return nil
}

// readSnapshotMember consumes one known, unique member without coercing its key.
func readSnapshotMember(decoder *json.Decoder, seen map[string]bool) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("snapshot member: %w", err)
	}
	key, ok := token.(string)
	if !ok || (key != "ipv4" && key != "ipv6" && key != "fetched_at") {
		return errors.New("snapshot contains an unknown member")
	}
	if seen[key] {
		return fmt.Errorf("snapshot contains duplicate %q", key)
	}
	seen[key] = true
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("snapshot %s: %w", key, err)
	}
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fmt.Errorf("snapshot %s must not be null", key)
	}
	return nil
}
