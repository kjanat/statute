package statute

// Listener is a surface listener declaration. Construct via HTTP or HTTPS.
type Listener struct {
	addr     string
	scheme   string // "http" or "https"
	redirect string // non-empty means this listener redirects to the named scheme

	autoTLS     *AutoTLSConfig
	staticTLS   *StaticTLSConfig
	enableHTTP2 bool
	http3Addr   string
	behindCF    bool
}

// HTTP starts an HTTP/1.1 listener declaration on the given address.
func HTTP(addr string) *Listener {
	return &Listener{addr: addr, scheme: schemeHTTP}
}

// HTTPS starts an HTTPS listener declaration on the given address. Options
// configure TLS material, HTTP/2, and HTTP/3.
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

// AutoTLSConfig declares ACME-managed TLS material.
type AutoTLSConfig struct {
	Domains []string
	email   string
	storage string
	dns01   *cloudflareDNS01Config
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

func (a *AutoTLSConfig) applyListener(l *Listener) { l.autoTLS = a }

// cloudflareDNS01Config carries the resolved-stage data for DNS-01.
type cloudflareDNS01Config struct {
	apiToken string
	zoneID   string
}

// StaticTLSConfig declares pre-provisioned TLS material.
type StaticTLSConfig struct {
	CertFile string
	KeyFile  string
}

// StaticTLS configures TLS using a static certificate and key on disk.
func StaticTLS(certFile, keyFile string) *StaticTLSConfig {
	return &StaticTLSConfig{CertFile: certFile, KeyFile: keyFile}
}

func (s *StaticTLSConfig) applyListener(l *Listener) { l.staticTLS = s }

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
func BehindCloudflare() ListenerOption { return behindCloudflareOption{} }

func (behindCloudflareOption) applyListener(l *Listener) { l.behindCF = true }
