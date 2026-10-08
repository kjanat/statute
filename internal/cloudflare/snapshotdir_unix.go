//go:build unix

package cloudflare

import (
	"errors"
	"os"
	"path/filepath"
)

func syncSnapshotDirectory(path string) error {
	dir, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	return syncCloseSnapshotDirectory(dir)
}

func syncCloseSnapshotDirectory(dir interface {
	Sync() error
	Close() error
},
) error {
	syncErr := dir.Sync()
	return errors.Join(syncErr, dir.Close())
}
