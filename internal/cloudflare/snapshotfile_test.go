package cloudflare

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteSnapshotDirectorySyncAfterReplacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fail bool
	}{
		{"success", false},
		{"sync failure", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot.json")
			snapshot := Bundled()
			if err := WriteSnapshot(path, snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot.FetchedAt = snapshot.FetchedAt.Add(time.Hour)
			syncFailure := errors.New("directory sync failure")
			calls := 0
			err := writeSnapshot(path, snapshot, func(dir string) error {
				calls++
				if dir != filepath.Dir(path) {
					t.Fatalf("sync directory = %q", dir)
				}
				assertSnapshotTime(t, path, snapshot.FetchedAt)
				if tc.fail {
					return syncFailure
				}
				return nil
			})
			if calls != 1 || errors.Is(err, syncFailure) != tc.fail || (err == nil) == tc.fail {
				t.Fatalf("sync calls=%d, error=%v, want failure=%v", calls, err, tc.fail)
			}
			if tc.fail && !strings.Contains(err.Error(), "snapshot replaced;") {
				t.Fatalf("error does not explain post-replacement failure: %v", err)
			}
			assertSnapshotTime(t, path, snapshot.FetchedAt)
			assertOnlySnapshot(t, filepath.Dir(path))
		})
	}
}

func assertSnapshotTime(t *testing.T, path string, want time.Time) {
	t.Helper()
	got, err := DecodeSnapshot(readSnapshot(t, path))
	if err != nil || !got.FetchedAt.Equal(want) {
		t.Fatalf("expected complete replacement with timestamp %s: %+v, %v", want, got, err)
	}
}

func TestWriteSnapshotRenameFailureSkipsDirectorySync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	err := writeSnapshot(path, Bundled(), func(string) error {
		t.Fatal("directory sync called after failed rename")
		return nil
	})
	var renameErr *os.LinkError
	if !errors.As(err, &renameErr) || renameErr.Op != "rename" {
		t.Fatalf("rename error not preserved: %v", err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("failed rename changed destination: %v, %v", info, statErr)
	}
	assertOnlySnapshot(t, dir)
}

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
