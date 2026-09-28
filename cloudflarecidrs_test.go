package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

// TestCloudflareCIDRsSnapshot pins the published source order and both address
// families without making the test suite depend on a live provider endpoint.
func TestCloudflareCIDRsSnapshot(t *testing.T) {
	t.Parallel()
	var want []string
	for _, family := range []struct {
		file  string
		count int
		is4   bool
	}{
		{"ips-v4.txt", 15, true},
		{"ips-v6.txt", 7, false},
	} {
		data, err := os.ReadFile("testdata/cloudflare/" + family.file)
		if err != nil {
			t.Fatal(err)
		}
		ranges := strings.Fields(string(data))
		if len(ranges) != family.count {
			t.Fatalf("%s: got %d ranges, want %d", family.file, len(ranges), family.count)
		}
		assertCloudflarePrefixFamily(t, ranges, family.is4)
		want = append(want, ranges...)
	}
	got := CloudflareCIDRs()
	if !slices.Equal(got, want) {
		t.Errorf("snapshot: got %v, want %v", got, want)
	}
	slices.Sort(got)
	if len(slices.Compact(got)) != len(want) {
		t.Error("snapshot contains duplicate prefixes")
	}
}

// assertCloudflarePrefixFamily checks the source's canonical CIDR spelling.
func assertCloudflarePrefixFamily(t *testing.T, ranges []string, is4 bool) {
	t.Helper()
	for _, cidr := range ranges {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if prefix.Masked().String() != cidr || prefix.Addr().Is4() != is4 {
			t.Errorf("noncanonical or wrong-family prefix %q (want IPv4=%v)", cidr, is4)
		}
	}
}

// TestCloudflareCIDRsIndependentCopies lets callers safely customize their
// policy without changing the preset or another listener's input.
func TestCloudflareCIDRsIndependentCopies(t *testing.T) {
	t.Parallel()
	want := CloudflareCIDRs()
	first, second := CloudflareCIDRs(), CloudflareCIDRs()
	first[0] = "192.0.2.0/24"
	first = append(first, "198.51.100.0/24")
	first[len(first)-1] = "203.0.113.0/24"
	if !slices.Equal(second, want) || !slices.Equal(CloudflareCIDRs(), want) {
		t.Fatal("editing or appending one result changed another result")
	}
}

// cloudflareConfig keeps the preset on an ordinary listener declaration.
func cloudflareConfig(tp *TrustedProxyConfig, behind bool) Config {
	opts := []ListenerOption{StaticTLS("cert.pem", "key.pem"), tp}
	if behind {
		opts = append(opts, BehindCloudflare())
	}
	return Config{
		Listeners: Listeners{HTTPS(":443", opts...)},
		Routes:    Routes{Match("/*").Serve("./public")},
	}
}

// TestCloudflareCIDRsResolveExport proves the helper feeds the existing
// normalized policy while preserving explicit header and TLS behavior choices.
func TestCloudflareCIDRsResolveExport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		header string
		behind bool
	}{
		{"default header", "", false},
		{"explicit header", "Cf-Connecting-Ip", false},
		{"behind cloudflare", "Cf-Connecting-Ip", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := TrustedProxy(CloudflareCIDRs()...)
			wantHeader := "X-Forwarded-For"
			if tc.header != "" {
				tp.ClientIPHeader(tc.header)
				wantHeader = tc.header
			}
			cfg := cloudflareConfig(tp, tc.behind)
			l := mustResolve(t, cfg).Listeners[0]
			assertCloudflareListener(t, l, wantHeader, tc.behind)
			var out bytes.Buffer
			if err := Export(cfg, &out); err != nil {
				t.Fatal(err)
			}
			var exported resolved.Config
			if err := json.Unmarshal(out.Bytes(), &exported); err != nil {
				t.Fatal(err)
			}
			assertCloudflareListener(t, exported.Listeners[0], wantHeader, tc.behind)
		})
	}
}

// assertCloudflareListener checks the normalized policy and independent TLS flag.
func assertCloudflareListener(t *testing.T, l *resolved.Listener, header string, behind bool) {
	t.Helper()
	if !slices.Equal(l.TrustedProxies, CloudflareCIDRs()) {
		t.Errorf("trusted ranges: %v", l.TrustedProxies)
	}
	if l.ClientIPHeader != header {
		t.Errorf("client header: got %q, want %q", l.ClientIPHeader, header)
	}
	if l.BehindCloudflare != behind {
		t.Errorf("BehindCloudflare: got %v, want %v", l.BehindCloudflare, behind)
	}
}

// TestCloudflareCIDRsBehindFlagDoesNotInstallPolicy preserves the existing
// flag-only contract: bundled peer verification is always an explicit choice.
func TestCloudflareCIDRsBehindFlagDoesNotInstallPolicy(t *testing.T) {
	t.Parallel()
	l := mustResolve(t, cloudflareConfig(nil, true)).Listeners[0]
	if len(l.TrustedProxies) != 0 || l.ClientIPHeader != "" || !l.BehindCloudflare {
		t.Fatalf("flag-only listener unexpectedly installed a proxy policy: %+v", l)
	}
}

// TestCloudflareCIDRsPeerTrust covers the real bundled prefixes, including
// mapped IPv4 peers, and prevents legacy fallbacks resurrecting refused headers.
func TestCloudflareCIDRsPeerTrust(t *testing.T) {
	t.Parallel()
	for _, behind := range []bool{false, true} {
		for _, tc := range []struct {
			name, remote, want string
			missing            bool
		}{
			{"ipv4", "173.245.48.1:443", "10.0.0.1", false},
			{"ipv6", "[2606:4700::1]:443", "10.0.0.1", false},
			{"mapped ipv4", "[::ffff:173.245.48.1]:443", "10.0.0.1", false},
			{"untrusted ipv4", "192.0.2.1:443", "192.0.2.1", false},
			{"untrusted ipv6", "[2001:db8::1]:443", "2001:db8::1", false},
			{"missing configured header", "173.245.48.1:443", "173.245.48.1", true},
		} {
			name := tc.name
			if behind {
				name += " behind cloudflare"
			}
			t.Run(name, func(t *testing.T) {
				l := mustResolve(t, cloudflareConfig(TrustedProxy(CloudflareCIDRs()...).ClientIPHeader("CF-Connecting-IP"), behind)).Listeners[0]
				var got string
				var h http.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = clientIP(r) })
				if behind {
					h = behindCloudflareMiddleware(h)
				}
				h = trustedProxyMiddleware(l, h)
				req := httptest.NewRequest(http.MethodGet, "https://x.example/", nil)
				req.RemoteAddr = tc.remote
				req.Header.Set("True-Client-IP", "10.0.0.2")
				req.Header.Set("X-Forwarded-For", "10.0.0.3")
				if !tc.missing {
					req.Header.Set("CF-Connecting-IP", "10.0.0.1")
				}
				h.ServeHTTP(httptest.NewRecorder(), req)
				if got != tc.want {
					t.Errorf("client IP: got %q, want %q", got, tc.want)
				}
			})
		}
	}
}

// TestCloudflareCIDRsOperatorPolicy preserves explicit append and replacement
// policies; a replacement must not secretly retain the bundled ranges.
func TestCloudflareCIDRsOperatorPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		cidrs      []string
		wantCFPeer bool
	}{
		{"append", append(CloudflareCIDRs(), "192.0.2.0/24"), true},
		{"replace", []string{"192.0.2.0/24"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := trustedListener(t, TrustedProxy(tc.cidrs...).ClientIPHeader("CF-Connecting-IP"))
			prefixes := mustParsePrefixes(l.TrustedProxies)
			if !addrInPrefixes(netip.MustParseAddr("192.0.2.1"), prefixes) {
				t.Error("operator range was lost")
			}
			if got := addrInPrefixes(netip.MustParseAddr("173.245.48.1"), prefixes); got != tc.wantCFPeer {
				t.Errorf("Cloudflare trust: got %v, want %v", got, tc.wantCFPeer)
			}
		})
	}
}

// TestCloudflareCIDRsListenerWiring exercises the actual TCP and HTTP/3 handler
// assembly and ensures another listener's explicit policy remains isolated.
func TestCloudflareCIDRsListenerWiring(t *testing.T) {
	t.Parallel()
	cert, key := writeSelfSignedCert(t, "x.example")
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "ok")
	r := mustResolve(t, Config{
		Listeners: Listeners{
			HTTPS(":443", StaticTLS(cert, key), HTTP3(":443/udp"), BehindCloudflare(), TrustedProxy(CloudflareCIDRs()...).ClientIPHeader("CF-Connecting-IP")),
			HTTPS(":8443", StaticTLS(cert, key), TrustedProxy("192.0.2.0/24").ClientIPHeader("CF-Connecting-IP")),
		},
		Routes: Routes{Match("/*").Serve(dir).With(AllowIPs("10.0.0.0/8"))},
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatal(err)
	}
	// This static-only configuration creates no pools, ACME workers or sockets.
	for _, tc := range []struct {
		name    string
		handler http.Handler
		remote  string
		want    int
	}{
		{"tcp trusted", srv.listeners[0].Handler, "173.245.48.1:443", http.StatusOK},
		{"quic trusted", srv.http3Servers[0].srv.Handler, "[2606:4700::1]:443", http.StatusOK},
		{"tcp spoof", srv.listeners[0].Handler, "198.51.100.1:443", http.StatusForbidden},
		{"quic spoof", srv.http3Servers[0].srv.Handler, "198.51.100.1:443", http.StatusForbidden},
		{"isolated rejects cloudflare", srv.listeners[1].Handler, "173.245.48.1:443", http.StatusForbidden},
		{"isolated accepts own proxy", srv.listeners[1].Handler, "192.0.2.1:443", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://x.example/", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("CF-Connecting-IP", "10.0.0.1")
			req.Header.Set("True-Client-IP", "10.0.0.1")
			req.Header.Set("X-Forwarded-For", "10.0.0.1")
			if rec := runRequest(t, tc.handler, req); rec.Code != tc.want {
				t.Errorf("status: got %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestCloudflareCIDRsALPN keeps ACME challenge policy independent of the preset.
func TestCloudflareCIDRsALPN(t *testing.T) {
	t.Parallel()
	for _, behind := range []bool{false, true} {
		opts := []ListenerOption{AutoTLS("x.example").Email("ops@example.com").Storage(t.TempDir()), TrustedProxy(CloudflareCIDRs()...).ClientIPHeader("CF-Connecting-IP")}
		if behind {
			opts = append(opts, BehindCloudflare())
		}
		r := mustResolve(t, Config{Listeners: Listeners{HTTPS(":443", opts...)}, Routes: Routes{Match("/*").Serve(t.TempDir())}})
		srv, err := newServer(r)
		if err != nil {
			t.Fatal(err)
		}
		// Construction alone does not start ACME workers, issue certificates or
		// bind listeners; inspecting ALPN needs neither a CA nor cleanup work.
		if got := slices.Contains(srv.listeners[0].TLSConfig.NextProtos, "acme-tls/1"); got == behind {
			t.Errorf("BehindCloudflare=%v: TLS-ALPN advertised=%v", behind, got)
		}
	}
}
