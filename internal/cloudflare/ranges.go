package cloudflare

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// IPv4URL and IPv6URL are the canonical published proxy-range sources.
const (
	IPv4URL = "https://www.cloudflare.com/ips-v4/#"
	IPv6URL = "https://www.cloudflare.com/ips-v6/#"
	maxBody = 64 << 10
)

// Snapshot contains one complete, validated pair of source lists.
type Snapshot struct {
	IPv4         []string      `json:"ipv4"`
	IPv6         []string      `json:"ipv6"`
	FetchedAt    time.Time     `json:"fetched_at"`
	RefreshAfter time.Duration `json:"-"`
}

// CIDRs returns a fresh concatenation in the sources' original order.
func (s Snapshot) CIDRs() []string {
	ranges := make([]string, 0, len(s.IPv4)+len(s.IPv6))
	ranges = append(ranges, s.IPv4...)
	return append(ranges, s.IPv6...)
}

//go:embed snapshot.json
var bundledJSON []byte

// Bundled returns independent copies of the release's generated fallback.
// An invalid embedded artifact is a build defect and cannot authorize traffic.
func Bundled() Snapshot {
	var snapshot Snapshot
	if err := json.Unmarshal(bundledJSON, &snapshot); err != nil {
		panic(fmt.Sprintf("cloudflare: invalid bundled snapshot: %v", err))
	}
	if err := snapshot.Validate(); err != nil {
		panic(fmt.Sprintf("cloudflare: invalid bundled snapshot: %v", err))
	}
	return snapshot
}

// Validate rejects incomplete, noncanonical or unsafe source snapshots.
func (s Snapshot) Validate() error {
	if s.FetchedAt.IsZero() {
		return errors.New("missing fetch timestamp")
	}
	if err := validateSnapshotList(s.IPv4, true); err != nil {
		return fmt.Errorf("IPv4: %w", err)
	}
	if err := validateSnapshotList(s.IPv6, false); err != nil {
		return fmt.Errorf("IPv6: %w", err)
	}
	return nil
}

// validateSnapshotList requires each stored element to contain exactly one
// canonical prefix; line parsing may normalize provider response whitespace.
func validateSnapshotList(ranges []string, is4 bool) error {
	parsed, err := parseList(strings.Join(ranges, "\n"), is4)
	if err != nil {
		return err
	}
	if !slices.Equal(parsed, ranges) {
		return errors.New("snapshot entries must each contain one canonical CIDR")
	}
	return nil
}

// NewClient creates a bounded client with a separately owned transport.
// The owner must call CloseIdleConnections after its final fetch.
func NewClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
		},
		Timeout:       5 * time.Second,
		CheckRedirect: rejectRedirect,
	}
}

// rejectRedirect keeps acquisition pinned to the canonical HTTPS endpoints.
func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("cloudflare range endpoint redirected")
}

// Fetch reads both canonical sources under the caller's total time budget.
// A failure returns no partial snapshot. Redirects are refused even when a
// test or maintenance caller supplies its own client.
func Fetch(ctx context.Context, client *http.Client) (Snapshot, error) {
	if client == nil {
		return Snapshot{}, errors.New("cloudflare range client is nil")
	}
	pinned := *client
	pinned.CheckRedirect = rejectRedirect
	v4, v4Next, err := fetchList(ctx, &pinned, IPv4URL, true)
	if err != nil {
		return Snapshot{}, err
	}
	v6, v6Next, err := fetchList(ctx, &pinned, IPv6URL, false)
	if err != nil {
		return Snapshot{}, err
	}
	now := time.Now().UTC()
	next := v4Next
	if v6Next.Before(next) {
		next = v6Next
	}
	return Snapshot{IPv4: v4, IPv6: v6, FetchedAt: now, RefreshAfter: clampRefresh(next.Sub(now))}, nil
}

// fetchList bounds the response before parsing any provider-controlled content.
func fetchList(ctx context.Context, client *http.Client, source string, is4 bool) ([]string, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("fetch %s: %w", source, err)
	}
	defer func() { _ = resp.Body.Close() }()
	received := time.Now()
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("fetch %s: HTTP %d", source, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read %s: %w", source, err)
	}
	if len(data) > maxBody {
		return nil, time.Time{}, fmt.Errorf("fetch %s: response exceeds %d bytes", source, maxBody)
	}
	prefixes, err := parseList(string(data), is4)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parse %s: %w", source, err)
	}
	return prefixes, received.Add(refreshAfter(resp.Header, received)), nil
}

// parseList accepts canonical, non-universal prefixes of exactly one family.
func parseList(body string, is4 bool) ([]string, error) {
	var prefixes []string
	seen := make(map[netip.Prefix]bool)
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(line)
		if err != nil {
			return nil, errors.New("invalid CIDR in range list")
		}
		if err := validatePrefix(prefix, line, is4, seen); err != nil {
			return nil, err
		}
		seen[prefix] = true
		prefixes = append(prefixes, line)
	}
	if len(prefixes) == 0 {
		return nil, errors.New("empty range list")
	}
	return prefixes, nil
}

// validatePrefix enforces the representation and trust scope of one entry.
func validatePrefix(prefix netip.Prefix, original string, is4 bool, seen map[netip.Prefix]bool) error {
	if prefix.Bits() == 0 {
		return errors.New("universal range is forbidden")
	}
	if prefix.Addr().Is4() != is4 || prefix.Addr().Is4In6() {
		return errors.New("wrong address family in range list")
	}
	if prefix.Masked().String() != original {
		return errors.New("noncanonical CIDR in range list")
	}
	if seen[prefix] {
		return errors.New("duplicate CIDR in range list")
	}
	return nil
}
