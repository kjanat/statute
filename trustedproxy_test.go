package statute

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

// trustedListener resolves the issue's example listener shape and returns
// the resolved form.
func trustedListener(t *testing.T, tp *TrustedProxyConfig) *resolved.Listener {
	t.Helper()
	r := mustResolve(t, Config{
		Listeners: Listeners{
			HTTPS(":443", StaticTLS("cert.pem", "key.pem"), tp),
		},
		Routes: Routes{Match("/*").Serve("./public")},
	})
	return r.Listeners[0]
}

// TestResolveTrustedProxy — CIDRs are canonicalised, the header defaults to
// X-Forwarded-For, and a configured header is stored in canonical form.
func TestResolveTrustedProxy(t *testing.T) {
	t.Parallel()
	l := trustedListener(t, TrustedProxy("203.0.113.7/24"))
	if len(l.TrustedProxies) != 1 || l.TrustedProxies[0] != "203.0.113.0/24" {
		t.Errorf("canonical CIDRs: %v", l.TrustedProxies)
	}
	if l.ClientIPHeader != "X-Forwarded-For" {
		t.Errorf("default header: %q", l.ClientIPHeader)
	}

	l = trustedListener(t, TrustedProxy("2001:db8::/32").ClientIPHeader("cf-connecting-ip"))
	if l.ClientIPHeader != "Cf-Connecting-Ip" {
		t.Errorf("canonical header: %q", l.ClientIPHeader)
	}

	untouched := trustedListener(t, nil)
	if untouched.TrustedProxies != nil || untouched.ClientIPHeader != "" {
		t.Errorf("listener without the option: %+v", untouched)
	}
}

// TestResolveTrustedProxyErrors — bad declarations fail at resolve time.
func TestResolveTrustedProxyErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tp      *TrustedProxyConfig
		wantErr string
	}{
		{"no cidrs", TrustedProxy(), "at least one CIDR"},
		{"bad cidr", TrustedProxy("not-a-cidr"), "trusted_proxy"},
		{"bad header", TrustedProxy("10.0.0.0/8").ClientIPHeader("X Bad"), "invalid character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(Config{
				Listeners: Listeners{HTTPS(":443", StaticTLS("c.pem", "k.pem"), c.tp)},
				Routes:    Routes{Match("/*").Serve("./public")},
			})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("got %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

// TestTrustedProxyClientIP — the policy decides per peer: a trusted peer
// speaks for its client through the forwarded header, everyone else is
// their own client and their headers are ignored.
func TestTrustedProxyClientIP(t *testing.T) {
	t.Parallel()
	l := &resolved.Listener{
		TrustedProxies: []string{"203.0.113.0/24"},
		ClientIPHeader: "X-Forwarded-For",
	}
	cases := []struct {
		name    string
		remote  string
		headers map[string][]string
		want    string
	}{
		{
			"trusted peer asserts the client",
			"203.0.113.7:4321",
			map[string][]string{"X-Forwarded-For": {"198.51.100.9"}},
			"198.51.100.9",
		},
		{
			"last value wins over a client-forged prefix",
			"203.0.113.7:4321",
			map[string][]string{"X-Forwarded-For": {"6.6.6.6, 198.51.100.9"}},
			"198.51.100.9",
		},
		{
			"last header line wins",
			"203.0.113.7:4321",
			map[string][]string{"X-Forwarded-For": {"6.6.6.6", "198.51.100.9"}},
			"198.51.100.9",
		},
		{
			"trusted peer without the header is itself the client",
			"203.0.113.7:4321",
			nil,
			"203.0.113.7:4321",
		},
		{
			"trusted peer with an empty value falls back to the peer",
			"203.0.113.7:4321",
			map[string][]string{"X-Forwarded-For": {"  "}},
			"203.0.113.7:4321",
		},
		{
			"untrusted peer cannot spoof through the header",
			"198.51.100.66:4321",
			map[string][]string{"X-Forwarded-For": {"10.0.0.1"}},
			"198.51.100.66:4321",
		},
		{
			"ipv4-mapped ipv6 peer matches an ipv4 range",
			"[::ffff:203.0.113.7]:4321",
			map[string][]string{"X-Forwarded-For": {"198.51.100.9"}},
			"198.51.100.9",
		},
		{
			"unparsable peer stays as-is",
			"pipe",
			map[string][]string{"X-Forwarded-For": {"10.0.0.1"}},
			"pipe",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var got string
			h := trustedProxyMiddleware(l, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = clientIP(r)
			}))
			req := httptest.NewRequest("GET", "http://x/", nil)
			req.RemoteAddr = c.remote
			maps.Copy(req.Header, c.headers)
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != c.want {
				t.Errorf("clientIP: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestTrustedProxyGovernsAlone — with a peer-trust policy on the listener,
// the blanket Cloudflare and X-Forwarded-For fallbacks in clientIP must not
// resurrect a header the policy refused for an untrusted peer.
func TestTrustedProxyGovernsAlone(t *testing.T) {
	t.Parallel()
	l := &resolved.Listener{
		TrustedProxies: []string{"203.0.113.0/24"},
		ClientIPHeader: "Cf-Connecting-Ip",
	}
	var got string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = clientIP(r)
	})
	// BehindCloudflare tags inside the policy, as buildListenerHandler wraps.
	h := trustedProxyMiddleware(l, behindCloudflareMiddleware(inner))

	req := httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "198.51.100.66:4321"
	req.Header.Set("CF-Connecting-IP", "10.0.0.1")
	req.Header.Set("X-Forwarded-For", "10.0.0.2")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "198.51.100.66:4321" {
		t.Errorf("untrusted peer: got %q, want the peer address", got)
	}

	// The same headers from a trusted peer resolve through the policy's
	// configured header, not the Cloudflare pair.
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("CF-Connecting-IP", "10.0.0.1")
	req.Header.Set("X-Forwarded-For", "10.0.0.2")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "10.0.0.1" {
		t.Errorf("trusted peer: got %q, want the configured header's value", got)
	}
}
