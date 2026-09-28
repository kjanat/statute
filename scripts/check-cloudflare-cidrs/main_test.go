package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestSourceClient(t *testing.T) {
	t.Parallel()
	c := sourceClient()
	if c.Timeout != 15*time.Second {
		t.Fatalf("timeout = %v", c.Timeout)
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect was not refused: %v", err)
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "matches", true: "drift"}[drift], func(t *testing.T) {
			t.Parallel()
			client, calls := fixtureClient(t, drift)
			var out bytes.Buffer
			err := check(t.Context(), client, &out)
			if drift {
				if err == nil || !strings.Contains(err.Error(), "snapshot differs") || !strings.Contains(err.Error(), "192.0.2.0/24") || out.Len() != 0 {
					t.Fatalf("drift: output=%q error=%v", out.String(), err)
				}
			} else if err != nil || !strings.Contains(out.String(), "matches both published lists") {
				t.Fatalf("match: output=%q error=%v", out.String(), err)
			}
			if *calls != 2 {
				t.Fatalf("requests = %d, want both source lists", *calls)
			}
		})
	}
}

func fixtureClient(t *testing.T, drift bool) (*http.Client, *int) {
	t.Helper()
	calls := 0
	client := sourceClient()
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "www.cloudflare.com" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		files := map[string]string{"/ips-v4/": "ips-v4.txt", "/ips-v6/": "ips-v6.txt"}
		file, ok := files[r.URL.Path]
		if !ok {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		data, err := os.ReadFile("../../testdata/cloudflare/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if drift && r.URL.Path == "/ips-v4/" {
			data = append(data, []byte("\n192.0.2.0/24")...)
		}
		return response(string(data), http.StatusOK), nil
	})
	return client, &calls
}

func TestCheckFetchFailure(t *testing.T) {
	t.Parallel()
	c := sourceClient()
	c.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response("unavailable", http.StatusServiceUnavailable), nil
	})
	var out bytes.Buffer
	err := check(t.Context(), c, &out)
	if err == nil || !strings.Contains(err.Error(), "https://www.cloudflare.com/ips-v4/#") || out.Len() != 0 {
		t.Fatalf("output=%q error=%v", out.String(), err)
	}
}

type brokenBody struct{ closed bool }

func (*brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (b *brokenBody) Close() error           { b.closed = true; return nil }

func TestFetchRejectsFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		code int
		want string
	}{
		{"status", "bad", http.StatusServiceUnavailable, "status 503"},
		{"redirect", "", http.StatusMovedPermanently, "status 301"},
		{"large", strings.Repeat("x", maxBody+1), http.StatusOK, "exceeds"},
		{"empty", " \n", http.StatusOK, "empty"},
		{"html", "<html>blocked</html>", http.StatusOK, "invalid prefix"},
		{"wrong family", "2400:cb00::/32", http.StatusOK, "wrong address family"},
		{"mapped", "::ffff:192.0.2.0/120", http.StatusOK, "wrong address family"},
		{"host bits", "192.0.2.1/24", http.StatusOK, "noncanonical"},
		{"duplicate", "192.0.2.0/24\n192.0.2.0/24", http.StatusOK, "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := sourceClient()
			c.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return response(tc.body, tc.code), nil })
			_, err := fetch(t.Context(), c, "https://www.cloudflare.com/ips-v4/#", true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestFetchReadErrorClosesBody(t *testing.T) {
	t.Parallel()
	body := &brokenBody{}
	c := sourceClient()
	c.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	})
	_, err := fetch(t.Context(), c, "https://www.cloudflare.com/ips-v4/#", true)
	if !errors.Is(err, io.ErrUnexpectedEOF) || !body.closed {
		t.Fatalf("error=%v, body closed=%v", err, body.closed)
	}
}

func TestFetchCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := sourceClient()
	c.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	_, err := fetch(ctx, c, "https://www.cloudflare.com/ips-v4/#", true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}
