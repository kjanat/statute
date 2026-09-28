package cloudflare

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// WriteSnapshot validates the complete pair before atomically replacing one file.
func WriteSnapshot(path string, snapshot Snapshot) error {
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
	return os.Rename(f.Name(), path)
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
