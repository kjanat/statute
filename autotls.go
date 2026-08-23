package statute

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/acme/autocert"

	"statute.kjanat.dev/resolved"
)

// buildAutocertManager scans every HTTP-01 AutoTLS source across all
// listeners and constructs a single autocert.Manager covering the union of
// their domains. DNS-01 sources are excluded — each gets its own cert
// manager (dns01Manager). Email and storage path must agree across all
// HTTP-01 sources; running multiple independent ACME accounts from a
// single binary is intentionally unsupported.
func buildAutocertManager(listeners []*resolved.Listener) (*autocert.Manager, error) {
	var (
		domains []string
		email   string
		storage string
		seen    bool
	)
	for _, l := range listeners {
		for _, a := range l.AutoTLSSources {
			// DNS-01 sources get their own manager.
			if a.DNS01 != nil {
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
