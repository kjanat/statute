package cloudflare

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteSnapshot validates and syncs a complete pair before replacing one file.
// Unix also syncs the parent directory; a subsequent error leaves the new file
// in place. Other platforms use their native rename guarantees.
func WriteSnapshot(path string, snapshot Snapshot) error {
	return writeSnapshot(path, snapshot, syncSnapshotDirectory)
}

func writeSnapshot(path string, snapshot Snapshot, syncDir func(string) error) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cloudflare-snapshot-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := prepareSnapshot(f, append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("snapshot replaced; parent directory durability unconfirmed: %w", err)
	}
	return nil
}

func prepareSnapshot(f *os.File, data []byte) error {
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
