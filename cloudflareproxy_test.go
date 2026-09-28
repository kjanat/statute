package statute

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"statute.kjanat.dev/internal/cloudflare"
	"statute.kjanat.dev/resolved"
)

func TestCloudflareTrustedProxyResolveExport(t *testing.T) {
	t.Parallel()
	for _, header := range []string{"CF-Connecting-IP", "X-Forwarded-For"} {
		cfg := cloudflareConfig(CloudflareTrustedProxy().ClientIPHeader(header), false)
		l := mustResolve(t, cfg).Listeners[0]
		assertDynamicCloudflareListener(t, l, header)
		var out bytes.Buffer
		if err := Export(cfg, &out); err != nil {
			t.Fatal(err)
		}
		var exported resolved.Config
		if err := json.Unmarshal(out.Bytes(), &exported); err != nil {
			t.Fatal(err)
		}
		assertDynamicCloudflareListener(t, exported.Listeners[0], header)
	}
	if _, err := Resolve(cloudflareConfig(CloudflareTrustedProxy().ClientIPHeader(""), false)); err == nil {
		t.Fatal("empty explicit header accepted")
	}
}

func assertDynamicCloudflareListener(t *testing.T, l *resolved.Listener, header string) {
	t.Helper()
	if !l.CloudflareTrustedProxy || len(l.TrustedProxies) != 0 || l.ClientIPHeader != http.CanonicalHeaderKey(header) || l.BehindCloudflare {
		t.Fatalf("dynamic policy: %+v", l)
	}
}

// cloudflareTestSnapshot supplies a complete hermetic provider response.
func cloudflareTestSnapshot(v4 string) cloudflare.Snapshot {
	return cloudflare.Snapshot{IPv4: []string{v4}, IPv6: []string{"2001:db8::/32"}, FetchedAt: time.Now().UTC(), RefreshAfter: time.Hour}
}

// controlledCloudflareWait exposes timer boundaries without wall-clock sleeps.
func controlledCloudflareWait(t *testing.T, source *cloudflareSource) (chan<- struct{}, <-chan time.Duration) {
	t.Helper()
	steps := make(chan struct{})
	delays := make(chan time.Duration, 8)
	source.wait = func(ctx context.Context, delay time.Duration) bool {
		select {
		case delays <- delay:
		case <-ctx.Done():
			return false
		}
		select {
		case <-steps:
			return true
		case <-ctx.Done():
			return false
		}
	}
	return steps, delays
}

func awaitCloudflareDelay(t *testing.T, delays <-chan time.Duration, want time.Duration) {
	t.Helper()
	select {
	case got := <-delays:
		if got != want {
			t.Fatalf("refresh delay: %s, want %s", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Cloudflare worker did not reach its timer")
	}
}

func stepCloudflareRefresh(t *testing.T, steps chan<- struct{}) {
	t.Helper()
	select {
	case steps <- struct{}{}:
	case <-time.After(3 * time.Second):
		t.Fatal("Cloudflare worker did not accept refresh tick")
	}
}

func TestCloudflareTrustedProxyRefreshRetainsLastGood(t *testing.T) {
	t.Parallel()
	source := newCloudflareSource()
	steps, delays := controlledCloudflareWait(t, source)
	calls := 0
	source.fetch = func(ctx context.Context, _ *http.Client) (cloudflare.Snapshot, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Error("fetch lacks bounded pair context")
		}
		calls++
		if calls == 2 {
			return cloudflare.Snapshot{}, errors.New("provider unavailable")
		}
		return cloudflareTestSnapshot("192.0.2.0/24"), nil
	}
	run := source.start()
	t.Cleanup(run.stop)
	awaitCloudflareDelay(t, delays, time.Hour)
	first := source.current.Load()
	stepCloudflareRefresh(t, steps)
	awaitCloudflareDelay(t, delays, cloudflareRetry)
	if source.current.Load() != first {
		t.Fatal("failed refresh discarded the last good snapshot")
	}
	stepCloudflareRefresh(t, steps)
	awaitCloudflareDelay(t, delays, time.Hour)
	if source.current.Load() == first {
		t.Fatal("successful refresh did not replace the snapshot")
	}
}

func TestCloudflareTrustedProxyFallbackWarning(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	source := newCloudflareSource()
	source.fetch = func(context.Context, *http.Client) (cloudflare.Snapshot, error) {
		return cloudflare.Snapshot{}, errors.New("test provider outage")
	}
	run := source.start()
	run.stop()
	if !slices.Equal(source.current.Load().prefixes, mustParsePrefixes(CloudflareCIDRs())) {
		t.Fatal("initial failure did not preserve complete bundled fallback")
	}
	if text := logs.String(); !strings.Contains(text, "test provider outage") || !strings.Contains(text, cloudflare.Bundled().FetchedAt.Format(time.RFC3339)) {
		t.Fatalf("missing fallback reason/date: %s", text)
	}
}

func TestCloudflareTrustedProxyRequestSnapshot(t *testing.T) {
	t.Parallel()
	source := newCloudflareSource()
	source.publish(cloudflareTestSnapshot("192.0.2.0/24"))
	h := source.middleware("Cf-Connecting-Ip", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		before := clientIP(r)
		source.publish(cloudflareTestSnapshot("198.51.100.0/24"))
		if after := clientIP(r); before != "10.0.0.1" || after != before {
			t.Errorf("request policy changed mid-request: before=%s after=%s", before, after)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "https://x.example/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("CF-Connecting-IP", "10.0.0.1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	var nextIP string
	source.middleware("Cf-Connecting-Ip", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { nextIP = clientIP(r) })).ServeHTTP(httptest.NewRecorder(), req)
	if nextIP != "192.0.2.1" {
		t.Fatalf("removed range still trusted on next request: %s", nextIP)
	}
}

func TestCloudflareTrustedProxyStopCancelsFetch(t *testing.T) {
	t.Parallel()
	source := newCloudflareSource()
	steps, delays := controlledCloudflareWait(t, source)
	entered := make(chan struct{})
	exited := make(chan struct{})
	calls := 0
	source.fetch = func(ctx context.Context, _ *http.Client) (cloudflare.Snapshot, error) {
		calls++
		if calls == 1 {
			return cloudflareTestSnapshot("192.0.2.0/24"), nil
		}
		close(entered)
		<-ctx.Done()
		defer close(exited)
		return cloudflare.Snapshot{}, ctx.Err()
	}
	run := source.start()
	t.Cleanup(run.stop)
	awaitCloudflareDelay(t, delays, time.Hour)
	stepCloudflareRefresh(t, steps)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("background fetch did not start")
	}
	if err := (&serverRun{cloudflare: run}).shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("stop returned with an in-flight fetch")
	}
}

// cloudflareTestServer constructs real TLS/HTTP3 handlers without starting them.
func cloudflareTestServer(t *testing.T, options ...ListenerOption) (*server, *http.Client) {
	t.Helper()
	cert, key := writeSelfSignedCert(t, "x.example")
	options = append([]ListenerOption{StaticTLS(cert, key)}, options...)
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "served")
	cfg := Config{Listeners: Listeners{HTTPS("127.0.0.1:0", options...)}, Routes: Routes{Match("/*").Serve(dir).With(AllowIPs("10.0.0.0/8"))}}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(filepath.Clean(cert))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("fixture certificate rejected")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "x.example", MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	return srv, &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func TestCloudflareTrustedProxyStartupRollbackRetry(t *testing.T) {
	t.Parallel()
	srv, client := cloudflareTestServer(t, CloudflareTrustedProxy())
	var calls atomic.Int32
	srv.cloudflare.fetch = func(context.Context, *http.Client) (cloudflare.Snapshot, error) {
		calls.Add(1)
		return cloudflareTestSnapshot("127.0.0.0/8"), nil
	}
	srv.listenTCP = func(context.Context, string, string) (net.Listener, error) {
		return nil, errors.New("injected bind failure")
	}
	// Capture the attempted worker through its wait callback; rollback must join it.
	waitExited := make(chan struct{})
	srv.cloudflare.wait = func(ctx context.Context, _ time.Duration) bool {
		<-ctx.Done()
		close(waitExited)
		return false
	}
	if err := srv.Start(); err == nil {
		t.Fatal("injected failed start succeeded")
	}
	select {
	case <-waitExited:
	default:
		t.Fatal("failed Start left refresh worker alive")
	}
	srv.cloudflare.wait = waitCloudflareRefresh
	srv.listenTCP = nil
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	firstRun := srv.run.cloudflare
	t.Cleanup(func() { _ = srv.Shutdown() })
	if calls.Load() != 2 || firstRun == nil {
		t.Fatalf("retry did not acquire a fresh run: calls=%d", calls.Load())
	}
	assertCloudflareServing(t, srv, client, http.StatusOK)
	if len(srv.cfg.Listeners[0].TrustedProxies) != 0 {
		t.Fatal("live ranges leaked into resolved config")
	}
}

func TestCloudflareTrustedProxyListenerWiring(t *testing.T) {
	t.Parallel()
	cert, key := writeSelfSignedCert(t, "x.example")
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "ok")
	cfg := Config{
		Listeners: Listeners{
			HTTPS("127.0.0.1:0", StaticTLS(cert, key), HTTP3("127.0.0.1:0/udp"), CloudflareTrustedProxy(), BehindCloudflare()),
			HTTPS("127.0.0.2:0", StaticTLS(cert, key), CloudflareTrustedProxy().ClientIPHeader("True-Client-IP")),
			HTTPS("127.0.0.3:0", StaticTLS(cert, key), TrustedProxy("203.0.113.0/24").ClientIPHeader("CF-Connecting-IP")),
		},
		Routes: Routes{Match("/*").Serve(dir).With(AllowIPs("10.0.0.0/8"))},
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv.cloudflare.fetch = func(context.Context, *http.Client) (cloudflare.Snapshot, error) {
		calls.Add(1)
		return cloudflareTestSnapshot("192.0.2.0/24"), nil
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	if calls.Load() != 1 {
		t.Fatalf("fetch pairs for two provider listeners: %d", calls.Load())
	}
	assertCloudflareDynamicHandlers(t, srv)
}

func assertCloudflareDynamicHandlers(t *testing.T, srv *server) {
	t.Helper()
	for _, tc := range []struct {
		name           string
		handler        http.Handler
		remote, header string
		want           int
	}{
		{"tcp v4", srv.listeners[0].Handler, "192.0.2.1:443", "CF-Connecting-IP", http.StatusOK},
		{"tcp mapped", srv.listeners[0].Handler, "[::ffff:192.0.2.1]:443", "CF-Connecting-IP", http.StatusOK},
		{"quic v6", srv.http3Servers[0].srv.Handler, "[2001:db8::1]:443", "CF-Connecting-IP", http.StatusOK},
		{"removed bundled", srv.listeners[0].Handler, "173.245.48.1:443", "CF-Connecting-IP", http.StatusForbidden},
		{"quic spoof", srv.http3Servers[0].srv.Handler, "198.51.100.1:443", "CF-Connecting-IP", http.StatusForbidden},
		{"own header", srv.listeners[1].Handler, "192.0.2.1:443", "True-Client-IP", http.StatusOK},
		{"other header", srv.listeners[1].Handler, "192.0.2.1:443", "CF-Connecting-IP", http.StatusForbidden},
		{"static independent", srv.listeners[2].Handler, "192.0.2.1:443", "CF-Connecting-IP", http.StatusForbidden},
		{"static own proxy", srv.listeners[2].Handler, "203.0.113.1:443", "CF-Connecting-IP", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://x.example/", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set(tc.header, "10.0.0.1")
			if rec := runRequest(t, tc.handler, req); rec.Code != tc.want {
				t.Errorf("HTTP %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func assertCloudflareServing(t *testing.T, srv *server, client *http.Client, want int) {
	t.Helper()
	url := "https://" + srv.run.listeners.http[0].listener.Addr().String() + "/"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("CF-Connecting-IP", "10.0.0.1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want || (want == http.StatusOK && string(body) != "served") {
		t.Fatalf("serving: HTTP %d %q, want %d", resp.StatusCode, body, want)
	}
}

func TestCloudflareTrustedProxyOptInOnly(t *testing.T) {
	t.Parallel()
	for _, options := range [][]ListenerOption{nil, {BehindCloudflare()}, {TrustedProxy(CloudflareCIDRs()...)}} {
		srv, _ := cloudflareTestServer(t, options...)
		if srv.cloudflare != nil {
			t.Fatal("ordinary/static/BehindCloudflare-only config created refresh source")
		}
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		if srv.run.cloudflare != nil {
			t.Error("unconfigured Cloudflare worker started")
		}
		if err := srv.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloudflareRefreshTimer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if waitCloudflareRefresh(ctx, time.Hour) {
		t.Fatal("canceled timer requested a refresh")
	}
	if !waitCloudflareRefresh(t.Context(), time.Nanosecond) {
		t.Fatal("elapsed timer did not request a refresh")
	}
}
