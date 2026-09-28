package cloudflare

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	io.Reader
	closed bool
}

type failedRangeReader struct{}

func (failedRangeReader) Read([]byte) (int, error) {
	return 0, errors.New("range response read failed")
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestFetchCompletePair(t *testing.T) {
	t.Parallel()
	var urls []string
	var bodies []*trackedBody
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		urls = append(urls, r.URL.String())
		if r.Method != http.MethodGet {
			t.Errorf("method: %s", r.Method)
		}
		data := "192.0.2.0/24\n198.51.100.0/24\n"
		cache := "s-maxage=3600"
		if r.URL.Path == "/ips-v6/" {
			data = "2001:db8::/32\n"
			cache = "max-age=7200"
		}
		body := &trackedBody{Reader: strings.NewReader(data)}
		bodies = append(bodies, body)
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: http.Header{"Cache-Control": {cache}}}, nil
	})}
	before := time.Now()
	snapshot, err := Fetch(t.Context(), client)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(urls, []string{strings.TrimSuffix(IPv4URL, "#"), strings.TrimSuffix(IPv6URL, "#")}) {
		t.Errorf("sources: %v", urls)
	}
	if got := snapshot.CIDRs(); !reflect.DeepEqual(got, []string{"192.0.2.0/24", "198.51.100.0/24", "2001:db8::/32"}) {
		t.Errorf("ranges: %v", got)
	}
	assertSnapshotTiming(t, snapshot, before)
	for _, body := range bodies {
		if !body.closed {
			t.Error("response body not closed")
		}
	}
}

func assertSnapshotTiming(t *testing.T, snapshot Snapshot, before time.Time) {
	t.Helper()
	if snapshot.FetchedAt.Before(before) || snapshot.FetchedAt.After(time.Now()) {
		t.Errorf("invalid fetch time: %s", snapshot.FetchedAt)
	}
	if snapshot.RefreshAfter > time.Hour || snapshot.RefreshAfter < time.Hour-time.Second {
		t.Errorf("earliest source refresh deadline lost: %s", snapshot.RefreshAfter)
	}
}

func TestFetchRejectsIncompleteOrInvalidPair(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		v4   string
		v6   string
	}{
		{"empty v4", "", "2001:db8::/32"},
		{"empty v6", "192.0.2.0/24", ""},
		{"bad syntax", "192.0.2.0/24", "<html>error</html>"},
		{"wrong family", "2001:db8::/32", "2001:db8::/32"},
		{"mapped family", "192.0.2.0/24", "::ffff:192.0.2.0/120"},
		{"noncanonical network", "192.0.2.1/24", "2001:db8::/32"},
		{"duplicate", "192.0.2.0/24\n192.0.2.0/24", "2001:db8::/32"},
		{"universal v4", "0.0.0.0/0", "2001:db8::/32"},
		{"universal v6", "192.0.2.0/24", "::/0"},
		{"oversized", strings.Repeat(" ", maxBody+1), "2001:db8::/32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				body := tc.v4
				if r.URL.Path == "/ips-v6/" {
					body = tc.v6
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			got, err := Fetch(t.Context(), client)
			if err == nil || len(got.CIDRs()) != 0 || !got.FetchedAt.IsZero() {
				t.Fatalf("invalid pair yielded %+v, %v", got, err)
			}
		})
	}
}

func TestFetchHTTPFailures(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusFound, http.StatusNotModified, http.StatusInternalServerError} {
		calls := 0
		body := &trackedBody{Reader: strings.NewReader("")}
		client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: code, Body: body, Header: http.Header{"Location": {"https://elsewhere.invalid/"}}}, nil
		})}
		if _, err := Fetch(t.Context(), client); err == nil {
			t.Errorf("HTTP %d accepted", code)
		}
		if calls != 1 || !body.closed {
			t.Errorf("HTTP %d: calls=%d, closed=%v", code, calls, body.closed)
		}
	}
}

func TestFetchCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	if _, err := Fetch(ctx, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fetch: %v", err)
	}
}

func TestFetchReadErrorAndNilClient(t *testing.T) {
	t.Parallel()
	if _, err := Fetch(t.Context(), nil); err == nil {
		t.Fatal("nil client accepted")
	}
	body := &trackedBody{Reader: failedRangeReader{}}
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}
	got, err := Fetch(t.Context(), client)
	if err == nil || len(got.CIDRs()) != 0 {
		t.Fatalf("read failure published snapshot: %+v, %v", got, err)
	}
	if !body.closed {
		t.Fatal("failed response body not closed")
	}
}

func TestBundledAndCopies(t *testing.T) {
	t.Parallel()
	snapshot := Bundled()
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.IPv4) == 0 || len(snapshot.IPv6) == 0 {
		t.Fatalf("unexpected snapshot families: %+v", snapshot)
	}
	want := Bundled()
	snapshot.IPv4[0] = "192.0.2.0/24"
	snapshot.IPv6[0] = "2001:db8::/32"
	copyRanges := want.CIDRs()
	copyRanges[0] = "198.51.100.0/24"
	if !reflect.DeepEqual(Bundled(), want) {
		t.Fatal("bundled snapshot shares mutable slices")
	}
}

func TestNewClientOwnsVerifiedTransport(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("range fetch inherited the global transport")
		return nil, errors.New("unexpected global transport call")
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	client := NewClient()
	t.Cleanup(client.CloseIdleConnections)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("owned transport has type %T", client.Transport)
	}
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("range client disabled TLS verification")
	}
	if client.Timeout != 5*time.Second || transport.DialContext == nil || transport.TLSHandshakeTimeout != 5*time.Second {
		t.Fatal("range client lacks bounded dial/TLS/request ownership")
	}
	if client.CheckRedirect == nil {
		t.Fatal("range client allows endpoint redirects")
	}
}

func TestSnapshotValidationRejectsUnnormalizedElements(t *testing.T) {
	t.Parallel()
	for _, family := range []struct {
		first, second string
		is4           bool
	}{
		{"192.0.2.0/24", "198.51.100.0/24", true},
		{"2001:db8::/32", "2606:4700::/32", false},
	} {
		for _, entries := range [][]string{
			{family.first, ""},
			{" " + family.first},
			{family.first + "\n" + family.second},
			{family.first + "\n"},
		} {
			snapshot := Snapshot{IPv4: []string{"192.0.2.0/24"}, IPv6: []string{"2001:db8::/32"}, FetchedAt: time.Now()}
			if family.is4 {
				snapshot.IPv4 = entries
			} else {
				snapshot.IPv6 = entries
			}
			if err := snapshot.Validate(); err == nil {
				t.Errorf("accepted unnormalized snapshot entries %q", entries)
			}
		}
	}
}
