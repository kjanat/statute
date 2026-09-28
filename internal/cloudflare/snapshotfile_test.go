package cloudflare

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readSnapshot(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertOnlySnapshot(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "snapshot.json" {
		t.Fatalf("temporary artifact leaked: %v, %v", entries, err)
	}
}

func TestWriteSnapshotValidationAndReproducibility(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	snapshot := Bundled()
	if err := WriteSnapshot(path, snapshot); err != nil {
		t.Fatal(err)
	}
	original := readSnapshot(t, path)
	if err := WriteSnapshot(path, snapshot); err != nil {
		t.Fatal(err)
	}
	invalid := snapshot
	invalid.IPv6 = nil
	if err := WriteSnapshot(path, invalid); err == nil {
		t.Fatal("accepted an incomplete pair")
	}
	if !bytes.Equal(original, readSnapshot(t, path)) {
		t.Fatal("repeated write or invalid snapshot changed artifact")
	}
	assertOnlySnapshot(t, filepath.Dir(path))
}

func TestWriteSnapshotMarshalFailurePreservesArtifact(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	snapshot := Bundled()
	if err := WriteSnapshot(path, snapshot); err != nil {
		t.Fatal(err)
	}
	original := readSnapshot(t, path)
	snapshot.FetchedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := WriteSnapshot(path, snapshot); err == nil {
		t.Fatal("unencodable timestamp was accepted")
	}
	if !bytes.Equal(original, readSnapshot(t, path)) {
		t.Fatal("encoding failure damaged existing fallback")
	}
	assertOnlySnapshot(t, filepath.Dir(path))
}

func TestPrepareSnapshotFileErrors(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := WriteSnapshot(path, Bundled()); err != nil {
		t.Fatal(err)
	}
	original := readSnapshot(t, path)
	readOnly, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readOnly.Close() })
	if err := prepareSnapshot(readOnly, []byte("incomplete")); err == nil {
		t.Fatal("write to read-only descriptor succeeded")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepareSnapshot(readOnly, []byte("incomplete")); err == nil {
		t.Fatal("closed descriptor succeeded")
	}
	if !bytes.Equal(original, readSnapshot(t, path)) {
		t.Fatal("failed write damaged the previous fallback")
	}
}
