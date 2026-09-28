// Command check-cloudflare-cidrs compares the bundled proxy ranges with
// Cloudflare's published lists. It never modifies the snapshot.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"statute.kjanat.dev"
)

const maxBody = 64 << 10

func main() {
	if err := check(context.Background(), sourceClient(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func sourceClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func check(ctx context.Context, client *http.Client, out io.Writer) error {
	var published []string
	for _, source := range []struct {
		url  string
		ipv4 bool
	}{
		{"https://www.cloudflare.com/ips-v4/#", true},
		{"https://www.cloudflare.com/ips-v6/#", false},
	} {
		ranges, err := fetch(ctx, client, source.url, source.ipv4)
		if err != nil {
			return fmt.Errorf("%s: %w", source.url, err)
		}
		published = append(published, ranges...)
	}
	if bundled := statute.CloudflareCIDRs(); !slices.Equal(bundled, published) {
		return fmt.Errorf("bundled Cloudflare CIDR snapshot differs:\nbundled: %v\npublished: %v\nreview the changes using docs/cloudflare.md#maintaining-the-bundled-snapshot; no files were changed", bundled, published)
	}
	_, err := fmt.Fprintf(out, "Cloudflare CIDR snapshot matches both published lists (%d prefixes).\n", len(published))
	return err
}

func fetch(ctx context.Context, client *http.Client, url string, ipv4 bool) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("response exceeds %d bytes", maxBody)
	}
	return parseRanges(string(body), ipv4)
}

func parseRanges(body string, ipv4 bool) ([]string, error) {
	ranges := strings.Fields(body)
	if len(ranges) == 0 {
		return nil, fmt.Errorf("empty range list")
	}
	seen := make(map[string]bool, len(ranges))
	for _, cidr := range ranges {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid prefix %q: %w", cidr, err)
		}
		if prefix.Addr().Is4() != ipv4 || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("wrong address family: %q", cidr)
		}
		if prefix.Masked().String() != cidr {
			return nil, fmt.Errorf("noncanonical prefix: %q", cidr)
		}
		if seen[cidr] {
			return nil, fmt.Errorf("duplicate prefix: %q", cidr)
		}
		seen[cidr] = true
	}
	return ranges, nil
}
