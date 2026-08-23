package statute

// Listener is a surface listener declaration. Construct via HTTP or HTTPS.
type Listener struct {
	addr     string
	scheme   string // "http" or "https"
	redirect string // non-empty means this listener redirects to the named scheme

	autoTLS      []*AutoTLSConfig
	staticTLS    []*StaticTLSConfig
	enableHTTP2  bool
	http3Addr    string
	behindCF     bool
	trustedProxy *TrustedProxyConfig
}

// HTTP starts an HTTP/1.1 listener declaration on the given address.
func HTTP(addr string) *Listener {
	return &Listener{addr: addr, scheme: schemeHTTP}
}

// HTTPS starts an HTTPS listener declaration on the given address. Options
// configure TLS material, HTTP/2, and HTTP/3.
//
// A listener accepts any number of TLS sources — AutoTLS, StaticTLS, and
// StaticTLSFor may be mixed freely — and selects one per handshake by SNI
// hostname: an exact name wins over a wildcard pattern, and a hostless
// StaticTLS acts as the fallback for names no source covers. Mixed
// public/direct names, DNS-01 wildcards, and externally provisioned
// certificates can therefore share one port:
//
//	statute.HTTPS(":443",
//	    statute.AutoTLS("foo.example.com").HTTP01(),
//	    statute.AutoTLS("*.bar.example").CloudflareDNS01(token),
//	    statute.StaticTLSFor("baz.example.net", certFile, keyFile),
//	)
func HTTPS(addr string, opts ...ListenerOption) *Listener {
	l := &Listener{addr: addr, scheme: schemeHTTPS}
	for _, o := range opts {
		o.applyListener(l)
	}
	return l
}

// RedirectTo turns this listener into a permanent redirect to the named scheme.
// The listener will not serve content beyond the redirect.
func (l *Listener) RedirectTo(scheme string) *Listener {
	l.redirect = scheme
	return l
}

// ListenerOption configures an HTTPS listener.
type ListenerOption interface {
	applyListener(l *Listener)
}

// AutoTLSConfig declares ACME-managed TLS material. A listener may carry
// several — each is one certificate source scoped to its domains.
type AutoTLSConfig struct {
	Domains        []string
	email          string
	storage        string
	dns01          *cloudflareDNS01Config
	explicitHTTP01 bool
}

// AutoTLS configures ACME auto-provisioning for the given domains.
func AutoTLS(domains ...string) *AutoTLSConfig {
	return &AutoTLSConfig{Domains: append([]string(nil), domains...)}
}

// Email sets the contact email registered with the ACME directory.
func (a *AutoTLSConfig) Email(email string) *AutoTLSConfig {
	a.email = email
	return a
}

// Storage sets the on-disk path where issued certificates and ACME state are
// persisted. Required for production use.
func (a *AutoTLSConfig) Storage(path string) *AutoTLSConfig {
	a.storage = path
	return a
}

// CloudflareDNS01 switches the ACME challenge from HTTP-01 to DNS-01 using
// Cloudflare's DNS API. Required for wildcard certificates and useful when
// :80 is not reachable from the public internet (private networks,
// Cloudflare-only origins, etc.).
//
// The token must be a Cloudflare API Token (not the legacy Global API Key)
// with the Zone.DNS:Edit permission for the zone(s) covering the listener's
// domains. Generate one at https://dash.cloudflare.com/profile/api-tokens.
//
// The zone is auto-discovered from each domain by walking the DNS labels
// against the account's zone list. Use Zone() to pin a specific zone ID and
// skip discovery.
//
// Returns the parent AutoTLSConfig so the call chain remains a single
// ListenerOption value usable as an argument to HTTPS.
func (a *AutoTLSConfig) CloudflareDNS01(apiToken string) *AutoTLSConfig {
	a.dns01 = &cloudflareDNS01Config{apiToken: apiToken}
	return a
}

// Zone pins the Cloudflare zone ID for DNS-01 challenges. Must be called
// after CloudflareDNS01. When unset the zone is discovered by querying
// Cloudflare for the zone whose name is a suffix of each domain.
func (a *AutoTLSConfig) Zone(id string) *AutoTLSConfig {
	if a.dns01 != nil {
		a.dns01.zoneID = id
	}
	return a
}

// HTTP01 pins this source to the HTTP-01 challenge. HTTP-01 is already the
// default when CloudflareDNS01 is not called, so the method changes nothing
// at runtime; it makes the challenge policy explicit where a listener mixes
// sources. Calling both HTTP01 and CloudflareDNS01 on one source is a
// resolve error rather than a silent precedence choice.
func (a *AutoTLSConfig) HTTP01() *AutoTLSConfig {
	a.explicitHTTP01 = true
	return a
}

func (a *AutoTLSConfig) applyListener(l *Listener) { l.autoTLS = append(l.autoTLS, a) }

// cloudflareDNS01Config carries the resolved-stage data for DNS-01.
type cloudflareDNS01Config struct {
	apiToken string
	zoneID   string
}

// StaticTLSConfig declares pre-provisioned TLS material. A listener may
// carry several; Host scopes each to one SNI name.
type StaticTLSConfig struct {
	CertFile string
	KeyFile  string

	// Host scopes the certificate to one SNI name — an exact hostname or a
	// wildcard pattern like "*.bar.example". Empty makes the source the
	// listener's fallback for unmatched names and SNI-less clients; at most
	// one hostless source may exist per listener.
	Host string

	hostSet bool
}

// StaticTLS configures TLS using a static certificate and key on disk. The
// source carries no hostname: alone on a listener it serves everything, and
// alongside other sources it is the fallback for names none of them cover.
func StaticTLS(certFile, keyFile string) *StaticTLSConfig {
	return &StaticTLSConfig{CertFile: certFile, KeyFile: keyFile}
}

// StaticTLSFor configures a static certificate served only for the given
// SNI hostname — an exact name or a wildcard pattern like "*.bar.example"
// covering exactly one extra label. An empty host is a resolve error; use
// StaticTLS for the hostless fallback.
func StaticTLSFor(host, certFile, keyFile string) *StaticTLSConfig {
	return &StaticTLSConfig{Host: host, CertFile: certFile, KeyFile: keyFile, hostSet: true}
}

func (s *StaticTLSConfig) applyListener(l *Listener) { l.staticTLS = append(l.staticTLS, s) }

type http2Option struct{}

// HTTP2 enables HTTP/2 on the listener. Required for h2 ALPN negotiation.
func HTTP2() ListenerOption { return http2Option{} }

func (http2Option) applyListener(l *Listener) { l.enableHTTP2 = true }

type http3Option struct{ addr string }

// HTTP3 enables HTTP/3 (QUIC) on the listener at the given UDP address.
// The addr should typically match the HTTPS port suffixed with /udp,
// for example ":443/udp".
func HTTP3(addr string) ListenerOption { return http3Option{addr: addr} }

func (h http3Option) applyListener(l *Listener) { l.http3Addr = h.addr }

// TrustedProxyConfig marks CIDR ranges whose members may assert the real
// client IP through a forwarded header. Build it with TrustedProxy.
type TrustedProxyConfig struct {
	cidrs  []string
	header string
}

// TrustedProxy trusts the given CIDR ranges as forwarding proxies on this
// listener. When the direct peer of a connection falls inside one of the
// ranges, the client IP is read from the forwarded header (ClientIPHeader,
// X-Forwarded-For by default); every other peer is its own client and its
// forwarded headers are ignored. Direct clients and proxied traffic can
// therefore share one listener without the headers becoming spoofable.
//
//	statute.HTTPS(":443",
//	    statute.TrustedProxy("203.0.113.0/24").ClientIPHeader("CF-Connecting-IP"),
//	)
//
// Of a multi-valued header, the last value counts: with one layer of
// trusted proxies that is the address the proxy itself observed, while the
// earlier values arrived from outside and remain client-controlled.
func TrustedProxy(cidrs ...string) *TrustedProxyConfig {
	return &TrustedProxyConfig{cidrs: cidrs, header: "X-Forwarded-For"}
}

// ClientIPHeader names the forwarded header consulted when the direct peer
// is a trusted proxy. Defaults to X-Forwarded-For; a Cloudflare-fronted
// listener typically wants CF-Connecting-IP. An empty name is a resolve
// error, not a fallback to the default — a header name that went missing
// (an unset environment variable, say) must not silently change which
// header is trusted.
func (t *TrustedProxyConfig) ClientIPHeader(name string) *TrustedProxyConfig {
	t.header = name
	return t
}

func (t *TrustedProxyConfig) applyListener(l *Listener) { l.trustedProxy = t }

type behindCloudflareOption struct{}

// BehindCloudflare marks the listener as sitting behind a Cloudflare proxy.
// This affects two things:
//
// First, when AutoTLS is configured on the listener, the TLS-ALPN-01 challenge
// is suppressed (the "acme-tls/1" entry is dropped from ALPN). Cloudflare
// terminates TLS at its edge and does not forward custom ALPN protocols, so
// TLS-ALPN-01 cannot succeed. Provisioning falls back to HTTP-01, which is
// served by the redirect listener on :80 — Cloudflare proxies that path
// transparently provided "Always Use HTTPS" is disabled for
// /.well-known/acme-challenge/*.
//
// Second, the request handling path trusts the CF-Connecting-IP and
// True-Client-IP headers as the originating client address. Other proxy
// headers (X-Forwarded-For) remain available but Cloudflare's are preferred
// because they are populated by the proxy and not user-controllable.
//
// That trust applies to every connection on the listener. When the same
// listener also receives direct traffic — a proxy-fronted hostname and a
// direct-origin hostname sharing an address — add TrustedProxy with
// Cloudflare's ranges and ClientIPHeader("CF-Connecting-IP") alongside this
// option: the trust policy then governs client IPs per peer, while
// BehindCloudflare keeps doing its ACME job of suppressing TLS-ALPN-01,
// which Cloudflare's edge cannot forward. Dropping BehindCloudflare in
// favour of TrustedProxy alone would re-advertise acme-tls/1 and can break
// AutoTLS issuance behind Cloudflare.
func BehindCloudflare() ListenerOption { return behindCloudflareOption{} }

func (behindCloudflareOption) applyListener(l *Listener) { l.behindCF = true }
