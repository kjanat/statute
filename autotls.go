package statute

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/acme/autocert"

	"statute.kjanat.dev/resolved"
)

// buildAutocertManager scans every automatic-challenge AutoTLS source
// across all listeners and constructs a single autocert.Manager covering
// the union of their domains. Pinned sources are excluded — DNS-01 and
// explicit HTTP-01 each get their own in-tree acmeManager, because
// autocert's challenge preference is hard-coded (TLS-ALPN-01 first) and
// cannot be pinned. Email and storage path must agree across all automatic
// sources; running multiple independent ACME accounts from one autocert
// manager is intentionally unsupported.
func buildAutocertManager(listeners []*resolved.Listener) (*autocert.Manager, error) {
	var (
		domains []string
		email   string
		storage string
		seen    bool
	)
	for _, l := range listeners {
		for _, a := range l.AutoTLSSources {
			// Pinned sources get their own in-tree manager.
			if a.DNS01 != nil || a.Challenge == resolved.ChallengeHTTP01 {
				continue
			}
			if !seen {
				email = a.Email
				storage = a.Storage
				seen = true
			} else {
				if email != a.Email {
					return nil, fmt.Errorf("auto_tls: email mismatch across sources (%q vs %q)", email, a.Email)
				}
				if storage != a.Storage {
					return nil, fmt.Errorf("auto_tls: storage mismatch across sources (%q vs %q)", storage, a.Storage)
				}
			}
			domains = append(domains, a.Domains...)
		}
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
