package statute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/internal/cloudflare"
	"statute.kjanat.dev/resolved"
)

func fallbackJSON(t *testing.T, snapshot cloudflare.Snapshot) []byte {
	t.Helper()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNewCloudflareSourceOwnsSelectedFallback(t *testing.T) {
	t.Parallel()
	fallback := cloudflare.Snapshot{
		IPv4:      []string{"198.51.100.0/24", "192.0.2.0/24"},
		IPv6:      []string{"2606:4700::/32", "2001:db8::/32"},
		FetchedAt: time.Date(2026, 9, 28, 14, 0, 0, 0, time.FixedZone("consumer", 2*60*60)),
	}
	want := fallback.Normalized()
	source := newCloudflareSource(fallback)
	fallback.IPv4[0] = "203.0.113.0/24"
	fallback.IPv6[0] = "2400:cb00::/32"
	fallback.FetchedAt = time.Time{}
	if !sameCloudflareFallback(source.fallback, want) || source.fallback.FetchedAt.Location() != time.UTC {
		t.Fatalf("constructor did not retain an independent normalized fallback: %+v", source.fallback)
	}
	published := source.current.Load()
	if !slices.Equal(published.prefixes, mustParsePrefixes(want.CIDRs())) || !published.fetchedAt.Equal(want.FetchedAt) {
		t.Fatalf("pre-Start publication differs from selected fallback: %+v", published)
	}
}

func TestCloudflareSourceSelectionBeforeStart(t *testing.T) {
	t.Parallel()
	custom := cloudflareTestSnapshot("192.0.2.0/24")
	static := &resolved.Listener{TrustedProxies: []string{"203.0.113.0/24"}}
	if source := cloudflareSourceForListeners([]*resolved.Listener{static}); source != nil {
		t.Fatal("static policy allocated a managed source")
	}
	for _, tc := range []struct {
		name     string
		fallback *resolved.CloudflareSnapshot
		want     cloudflare.Snapshot
	}{
		{"bundled", nil, cloudflare.Bundled().Normalized()},
		{"consumer", &resolved.CloudflareSnapshot{IPv4: custom.IPv4, IPv6: custom.IPv6, FetchedAt: custom.FetchedAt}, custom.Normalized()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := cloudflareSourceForListeners([]*resolved.Listener{static, {CloudflareTrustedProxy: true, CloudflareFallback: tc.fallback}})
			if source == nil {
				t.Fatal("managed policy did not allocate a source")
			}
			published := source.current.Load()
			if !slices.Equal(published.prefixes, mustParsePrefixes(tc.want.CIDRs())) || !published.fetchedAt.Equal(tc.want.FetchedAt) {
				t.Fatalf("pre-Start fallback selection: %+v", published)
			}
		})
	}
}

func TestCloudflareFallbackCopiesAndExports(t *testing.T) {
	t.Parallel()
	snapshot := cloudflareTestSnapshot("192.0.2.0/24")
	snapshot.IPv4 = append(snapshot.IPv4, "127.0.0.0/8")
	data := fallbackJSON(t, snapshot)
	policy := CloudflareTrustedProxy().FallbackSnapshot(data)
	clear(data)
	cfg := cloudflareConfig(policy, false)
	l := mustResolve(t, cfg).Listeners[0]
	if l.CloudflareFallback == nil || !slices.Equal(l.CloudflareFallback.IPv4, []string{"127.0.0.0/8", "192.0.2.0/24"}) {
		t.Fatalf("fallback was aliased or not normalized: %+v", l.CloudflareFallback)
	}
	l.CloudflareFallback.IPv4[0] = "203.0.113.0/24"
	var exported bytes.Buffer
	if err := Export(cfg, &exported); err != nil {
		t.Fatal(err)
	}
	var decoded resolved.Config
	if err := json.Unmarshal(exported.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded.Listeners[0].CloudflareFallback
	if got == nil || got.IPv4[0] != "127.0.0.0/8" || !got.FetchedAt.Equal(snapshot.FetchedAt) {
		t.Fatalf("exported fallback provenance changed: %+v", got)
	}
}

func TestCloudflareFallbackInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for _, policy := range []*TrustedProxyConfig{
		CloudflareTrustedProxy().FallbackSnapshot(nil),
		CloudflareTrustedProxy().FallbackSnapshot([]byte{}),
		CloudflareTrustedProxy().FallbackSnapshot([]byte("null")),
		TrustedProxy("192.0.2.0/24").FallbackSnapshot(fallbackJSON(t, cloudflareTestSnapshot("192.0.2.0/24"))),
	} {
		if _, err := Resolve(cloudflareConfig(policy, false)); err == nil {
			t.Fatal("invalid fallback configuration accepted")
		}
	}
}

func TestCloudflareFallbackAcrossListeners(t *testing.T) {
	t.Parallel()
	snapshot := cloudflareTestSnapshot("192.0.2.0/24")
	snapshot.IPv4 = append(snapshot.IPv4, "198.51.100.0/24")
	equivalent := snapshot.Normalized()
	slices.Reverse(equivalent.IPv4)
	equivalent.FetchedAt = equivalent.FetchedAt.In(time.FixedZone("consumer", 2*60*60))
	different := snapshot.Normalized()
	different.FetchedAt = different.FetchedAt.Add(time.Second)
	differentRanges := snapshot.Normalized()
	differentRanges.IPv4[0] = "203.0.113.0/24"
	differentIPv6 := snapshot.Normalized()
	differentIPv6.IPv6[0] = "2606:4700::/32"
	for _, tc := range []struct {
		name          string
		first, second *TrustedProxyConfig
		wantError     bool
	}{
		{"same ranges timezone order", CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, snapshot)), CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, equivalent)), false},
		{"different provenance", CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, snapshot)), CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, different)), true},
		{"different ranges same time", CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, snapshot)), CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, differentRanges)), true},
		{"different IPv6 same time", CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, snapshot)), CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, differentIPv6)), true},
		{"mixed conflicting", CloudflareTrustedProxy(), CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, snapshot)), true},
		{"mixed identical", CloudflareTrustedProxy(), CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, cloudflare.Bundled())), false},
		{"static unaffected", CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, snapshot)), TrustedProxy("203.0.113.0/24"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Listeners: Listeners{HTTPS(":443", StaticTLS("c", "k"), tc.first), HTTPS(":8443", StaticTLS("c", "k"), tc.second)},
				Routes:    Routes{Match("/*").Serve("./public")},
			}
			_, err := Resolve(cfg)
			if (err != nil) != tc.wantError {
				t.Fatalf("Resolve error=%v, wantError=%v", err, tc.wantError)
			}
			if err != nil && !strings.Contains(err.Error(), "listener[1]") {
				t.Fatalf("conflict omitted listener identity: %v", err)
			}
		})
	}
}

func TestCloudflareFallbackReseedsNextAttempt(t *testing.T) {
	t.Parallel()
	source := newCloudflareSource(cloudflareTestSnapshot("127.0.0.0/8"))
	calls := 0
	source.fetch = func(context.Context, *http.Client) (cloudflare.Snapshot, error) {
		calls++
		if calls == 1 {
			return cloudflareTestSnapshot("192.0.2.0/24"), nil
		}
		return cloudflare.Snapshot{}, errors.New("retry provider outage")
	}
	first := source.start()
	first.stop()
	second := source.start()
	t.Cleanup(second.stop)
	if !slices.Equal(source.current.Load().prefixes, mustParsePrefixes(source.fallback.CIDRs())) {
		t.Fatal("new attempt inherited stale live ranges instead of its configured fallback")
	}
}

// TestCloudflareFallbackServingAndRefresh proves custom fallback ownership across
// caller mutation, live replacement and a subsequent failed refresh.
func TestCloudflareFallbackServingAndRefresh(t *testing.T) {
	t.Parallel()
	snapshot := cloudflareTestSnapshot("127.0.0.0/8")
	srv, client := cloudflareTestServer(t, CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, snapshot)))
	// Runtime has already captured independent fallback state.
	srv.cfg.Listeners[0].CloudflareFallback.IPv4[0] = "203.0.113.0/24"
	srv.cfg.Listeners[0].CloudflareFallback.FetchedAt = time.Time{}
	steps, delays := controlledCloudflareWait(t, srv.cloudflare)
	calls := 0
	srv.cloudflare.fetch = func(context.Context, *http.Client) (cloudflare.Snapshot, error) {
		calls++
		if calls == 2 {
			return cloudflareTestSnapshot("192.0.2.0/24"), nil
		}
		return cloudflare.Snapshot{}, errors.New("test provider outage")
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	awaitCloudflareDelay(t, delays, cloudflareRetry)
	assertCloudflareServing(t, srv, client, http.StatusOK)
	if !srv.cloudflare.current.Load().fetchedAt.Equal(snapshot.FetchedAt) {
		t.Fatal("runtime lost captured fallback provenance")
	}
	stepCloudflareRefresh(t, steps)
	awaitCloudflareDelay(t, delays, time.Hour)
	assertCloudflareServing(t, srv, client, http.StatusForbidden)
	live := srv.cloudflare.current.Load()
	stepCloudflareRefresh(t, steps)
	awaitCloudflareDelay(t, delays, cloudflareRetry)
	assertCloudflareServing(t, srv, client, http.StatusForbidden)
	if srv.cloudflare.current.Load() != live {
		t.Fatal("failed refresh restored fallback over last good live ranges")
	}
}

func TestGraphDOTCloudflareFallbackProvenance(t *testing.T) {
	t.Parallel()
	snapshot := cloudflareTestSnapshot("192.0.2.0/24")
	var graph bytes.Buffer
	if err := GraphDOT(cloudflareConfig(CloudflareTrustedProxy().FallbackSnapshot(fallbackJSON(t, snapshot)), false), &graph); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fallback=consumer", snapshot.FetchedAt.Format(time.RFC3339Nano), "IPv4=1 IPv6=1", "startup + periodic"} {
		if !strings.Contains(graph.String(), want) {
			t.Errorf("graph omitted %q: %s", want, graph.String())
		}
	}
}
