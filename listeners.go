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
}

// HTTP starts an HTTP/1.1 listener declaration on the given address.
func HTTP(addr string) *Listener {
	return &Listener{addr: addr, scheme: "http"}
}

// HTTPS starts an HTTPS listener declaration on the given address. Options
// configure TLS material, HTTP/2, and HTTP/3.
func HTTPS(addr string, opts ...ListenerOption) *Listener {
	l := &Listener{addr: addr, scheme: "https"}
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

func (a *AutoTLSConfig) applyListener(l *Listener) { l.autoTLS = a }

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
