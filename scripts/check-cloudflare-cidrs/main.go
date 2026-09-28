// Command check-cloudflare-cidrs compares or regenerates the embedded fallback.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"statute.kjanat.dev/internal/cloudflare"
)

func main() {
	client := cloudflare.NewClient()
	err := run(context.Background(), client, os.Args[1:], os.Stdout)
	client.CloseIdleConnections()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, client *http.Client, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("check-cloudflare-cidrs", flag.ContinueOnError)
	flags.SetOutput(out)
	update := flags.Bool("update", false, "fetch and atomically regenerate the embedded fallback")
	output := flags.String("output", "internal/cloudflare/snapshot.json", "generated snapshot path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	published, err := cloudflare.Fetch(ctx, client)
	if err != nil {
		return err
	}
	if *update {
		if err := writeSnapshot(*output, published); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Generated %s (%d prefixes, fetched %s). Rebuild to embed this fallback.\n", *output, len(published.CIDRs()), published.FetchedAt.Format(time.RFC3339))
		return err
	}
	if bundled := cloudflare.Bundled().CIDRs(); !slices.Equal(bundled, published.CIDRs()) {
		return fmt.Errorf("bundled Cloudflare CIDR snapshot differs:\nbundled: %v\npublished: %v\nrun make generate-cloudflare-cidrs and review the generated fallback diff; no files were changed", bundled, published.CIDRs())
	}
	_, err = fmt.Fprintf(out, "Cloudflare CIDR snapshot matches both published lists (%d prefixes); next runtime refresh in %s.\n", len(published.CIDRs()), published.RefreshAfter.Round(time.Second))
	return err
}

// writeSnapshot validates the complete pair before atomically replacing one file.
func writeSnapshot(path string, snapshot cloudflare.Snapshot) error {
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
