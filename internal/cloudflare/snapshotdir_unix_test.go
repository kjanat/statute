//go:build unix

package cloudflare

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSyncSnapshotDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := syncSnapshotDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if err := syncSnapshotDirectory(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory error = %v", err)
	}
}

type snapshotDirectoryFaults struct {
	syncErr, closeErr error
	calls             []string
}

func (d *snapshotDirectoryFaults) Sync() error {
	d.calls = append(d.calls, "sync")
	return d.syncErr
}

func (d *snapshotDirectoryFaults) Close() error {
	d.calls = append(d.calls, "close")
	return d.closeErr
}

func TestSyncCloseSnapshotDirectory(t *testing.T) {
	t.Parallel()
	syncFailure := errors.New("sync failure")
	closeFailure := errors.New("close failure")
	for _, tc := range []struct {
		name              string
		syncErr, closeErr error
	}{
		{"success", nil, nil},
		{"sync failure", syncFailure, nil},
		{"close failure", nil, closeFailure},
		{"both fail", syncFailure, closeFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := &snapshotDirectoryFaults{syncErr: tc.syncErr, closeErr: tc.closeErr}
			err := syncCloseSnapshotDirectory(dir)
			if !slices.Equal(dir.calls, []string{"sync", "close"}) {
				t.Fatalf("operation order = %v", dir.calls)
			}
			if (err == nil) != (tc.syncErr == nil && tc.closeErr == nil) {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, want := range []error{tc.syncErr, tc.closeErr} {
				if want != nil && !errors.Is(err, want) {
					t.Fatalf("error %v lost %v", err, want)
				}
			}
		})
	}
}
