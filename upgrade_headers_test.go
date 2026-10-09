package statute

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

//nolint:gocyclo // handshake policy and bidirectional streaming share one real-socket assertion matrix.
func TestNativeUpgradeRouteHeaders(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSet-Cookie: session=origin\r\nX-Policy: origin\r\nX-Add: origin\r\nX-Request-Id: origin\r\nVary: Accept-Language\r\n\r\n")
		_ = rw.Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(rw, payload); err != nil {
			t.Error(err)
			return
		}
		_, _ = rw.Write(payload)
		_ = rw.Flush()
	}))
	t.Cleanup(backend.Close)
	policy := []Middleware{
		RequestID().From("X-Client-Id"), CORS().Origins("*"),
		RemoveResponseHeader("Set-Cookie"),
		SetResponseHeader("X-Policy", "route"),
		AddResponseHeader("X-Add", "route"),
		SetResponseHeader("X-Request-Id", "route-id"),
		RemoveResponseHeader("Vary"),
	}
	wrapped := append(slices.Clone(policy), Retry(2), Cache("1m"), ETag(), Compress())
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: backend.URL}}}},
		Routes: Routes{
			Match("/protected").ProxyTo("shared").With(policy...),
			Match("/wrapped").ProxyTo("shared").With(wrapped...),
			Match("/public").ProxyTo("shared"),
		},
		FallbackRoutes: Routes{Match("/*").ProxyTo("shared").With(policy...)},
	})
	front := httptest.NewServer(router)
	t.Cleanup(front.Close)
	for _, path := range []string{"/protected", "/wrapped", "/public", "/fallback"} {
		t.Run(path, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(front.URL, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: client\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nX-Client-Id: client-id\r\n\r\n", path)
			rd := bufio.NewReader(conn)
			resp, err := http.ReadResponse(rd, &http.Request{Method: http.MethodGet})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			if path == "/public" {
				assertHeader(t, resp.Header, "Set-Cookie", "session=origin")
				assertHeader(t, resp.Header, "X-Policy", "origin")
			} else {
				assertNoHeader(t, resp.Header, "Set-Cookie")
				if got := resp.Header.Values("X-Request-Id"); !slices.Equal(got, []string{"route-id"}) {
					t.Errorf("request ID: %v", got)
				}
				assertHeader(t, resp.Header, "X-Policy", "route")
				if got := resp.Header.Values("X-Add"); !slices.Equal(got, []string{"origin", "route"}) {
					t.Errorf("add: %v", got)
				}
				assertHeader(t, resp.Header, "Vary", "Origin")
			}
			_, _ = io.WriteString(conn, "ping")
			readExactWithin(t, rd, "ping", 4*time.Second)
		})
	}
}

// Failed upgrades retain socket ownership and cannot demote a shared pool
// merely because a route policy or downstream writer rejects the handshake.
//
//nolint:gocyclo // each failure control asserts socket retirement, restored policy and passive-health isolation.
func TestNativeFailedUpgradeClosesBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, remove, protocol string
		noHijack               bool
		failures               int
	}{
		{name: "remove-upgrade", remove: "Upgrade", protocol: "websocket"},
		{name: "remove-connection", remove: "Connection", protocol: "websocket"},
		{name: "no-hijacker", protocol: "websocket", noHijack: true},
		{name: "backend-mismatch", protocol: "other", failures: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := make(chan error, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, rw, err := http.NewResponseController(w).Hijack()
				if err != nil {
					closed <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", tc.protocol)
				_ = rw.Flush()
				_, err = rw.ReadByte()
				closed <- err
			}))
			t.Cleanup(backend.Close)
			pool := newPassivePoolHandler(t, nil, time.Minute, 1, backend.URL)
			run := pool.start()
			t.Cleanup(run.shutdown)
			var handler http.Handler = pool
			if tc.remove != "" {
				handler = withHeaderMiddleware([]resolved.Middleware{
					{Type: resolved.MWRemoveResponseHeader, HeaderName: tc.remove},
					{Type: resolved.MWSetResponseHeader, HeaderName: "X-Policy", HeaderValue: "retained"},
				}, handler)
			}
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.noHijack {
					w = &upgradeNoHijacker{ResponseWriter: w}
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(front.Close)
			req, err := http.NewRequest(http.MethodGet, front.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			if tc.remove != "" {
				assertHeader(t, resp.Header, "X-Policy", "retained")
			}
			select {
			case err := <-closed:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("backend socket not retired cleanly: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("failed upgrade left backend socket alive")
			}
			if got := passiveFailureCount(run.passive, pool.primary[0]); got != tc.failures {
				t.Fatalf("backend failures=%d; want %d", got, tc.failures)
			}
		})
	}
}

type upgradeNoHijacker struct{ http.ResponseWriter }
