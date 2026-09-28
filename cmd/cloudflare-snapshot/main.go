// Command cloudflare-snapshot generates an optional application-owned fallback.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
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
	flags := flag.NewFlagSet("cloudflare-snapshot", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("out", "cloudflare.json", "generated fallback JSON path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" {
		return fmt.Errorf("expected -out path and no positional arguments")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	snapshot, err := cloudflare.Fetch(ctx, client)
	if err != nil {
		return err
	}
	if err := cloudflare.WriteSnapshot(*path, snapshot); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Generated %s (%d prefixes, fetched %s). Rebuild to embed this fallback.\n", *path, len(snapshot.CIDRs()), snapshot.FetchedAt.Format(time.RFC3339))
	return err
}
