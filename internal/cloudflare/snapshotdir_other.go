//go:build !unix

package cloudflare

// Directory syncing is unavailable here; replacement uses native rename semantics.
func syncSnapshotDirectory(string) error {
	return nil
}
