package statute

import (
	"crypto/tls"
	"errors"
	"fmt"

	"golang.org/x/crypto/acme/autocert"

	"github.com/kjanat/statute/resolved"
)

// buildAutocertManager scans all HTTPS listeners with HTTP-01 AutoTLS and
// constructs a single autocert.Manager that covers the union of their
// domains. DNS-01 listeners are excluded — they use a separate
// per-listener cert manager (dns01Manager). Email and storage path must
// agree across all HTTP-01 AutoTLS listeners; running multiple independent
// ACME accounts from a single binary is intentionally unsupported.
func buildAutocertManager(listeners []*resolved.Listener) (*autocert.Manager, error) {
	var (
		domains []string
		email   string
		storage string
		seen    bool
	)
	for _, l := range listeners {
		if l.AutoTLS == nil {
			continue
		}
		// DNS-01 listeners get their own manager.
		if l.AutoTLS.DNS01 != nil {
			continue
		}
		if !seen {
			email = l.AutoTLS.Email
			storage = l.AutoTLS.Storage
			seen = true
		} else {
			if email != l.AutoTLS.Email {
				return nil, fmt.Errorf("auto_tls: email mismatch across listeners (%q vs %q)", email, l.AutoTLS.Email)
			}
			if storage != l.AutoTLS.Storage {
				return nil, fmt.Errorf("auto_tls: storage mismatch across listeners (%q vs %q)", storage, l.AutoTLS.Storage)
			}
		}
		domains = append(domains, l.AutoTLS.Domains...)
	}
	if !seen {
		return nil, nil
	}
	if storage == "" {
		return nil, errors.New("auto_tls: storage path is required")
	}
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Email:      email,
		Cache:      autocert.DirCache(storage),
		HostPolicy: autocert.HostWhitelist(domains...),
	}, nil
}

// autocertTLSConfig returns a *tls.Config that uses the given manager for
// dynamic certificate provisioning. ALPN advertises h2 when HTTP/2 is enabled
// on the listener; tls-alpn-01 is included so the manager can satisfy
// challenges without needing an HTTP-01 fallback — except when behindCF is
// true, in which case tls-alpn-01 is dropped because Cloudflare's edge
// terminates TLS and will not forward the custom ALPN protocol.
func autocertTLSConfig(m *autocert.Manager, http2, behindCF bool) *tls.Config {
	cfg := m.TLSConfig()
	protos := []string{alpnHTTP1}
	if http2 {
		protos = append([]string{alpnHTTP2}, protos...)
	}
	if !behindCF {
		protos = append(protos, alpnACMETLS)
	}
	cfg.NextProtos = protos
	return cfg
}
