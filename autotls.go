package statute

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"statute.kjanat.dev/resolved"
)

// buildAutocertManager scans every automatic-challenge AutoTLS source
// across all listeners and constructs a single autocert.Manager covering
// the union of their domains. Pinned sources are excluded — DNS-01 and
// explicit HTTP-01 each get their own in-tree acmeManager, because
// autocert's challenge preference is hard-coded (TLS-ALPN-01 first) and
// cannot be pinned. Email, storage path, and directory must agree across all automatic
// sources; running multiple independent ACME accounts from one autocert
// manager is intentionally unsupported.
func buildAutocertManager(listeners []*resolved.Listener) (*autocert.Manager, error) {
	sources := automaticAutoTLSSources(listeners)
	if len(sources) == 0 {
		return nil, nil
	}
	first := sources[0]
	var domains []string
	for _, a := range sources {
		if err := autocertSourceAgreement(first, a); err != nil {
			return nil, err
		}
		domains = append(domains, a.Domains...)
	}
	if first.Storage == "" {
		return nil, errors.New("auto_tls: storage path is required")
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Email:      first.Email,
		Cache:      autocert.DirCache(first.Storage),
		HostPolicy: autocert.HostWhitelist(domains...),
	}
	if first.Directory != "" && first.Directory != acme.LetsEncryptURL {
		// autocert defaults to Let's Encrypt when Client is nil; only a
		// non-default directory needs an explicit client.
		m.Client = &acme.Client{DirectoryURL: first.Directory}
	}
	return m, nil
}

// automaticAutoTLSSources collects the ChallengeAuto sources across all
// listeners — the ones served by the shared autocert manager. Pinned
// sources get their own in-tree manager.
func automaticAutoTLSSources(listeners []*resolved.Listener) []*resolved.AutoTLS {
	var sources []*resolved.AutoTLS
	for _, l := range listeners {
		for _, a := range l.AutoTLSSources {
			if a.DNS01 != nil || a.Challenge == resolved.ChallengeHTTP01 {
				continue
			}
			sources = append(sources, a)
		}
	}
	return sources
}

// autocertSourceAgreement rejects an automatic source that disagrees with
// the first one on the account-defining fields. All automatic sources feed
// one shared autocert manager, which holds a single ACME account.
func autocertSourceAgreement(first, a *resolved.AutoTLS) error {
	if first.Email != a.Email {
		return fmt.Errorf("auto_tls: email mismatch across sources (%q vs %q)", first.Email, a.Email)
	}
	if first.Storage != a.Storage {
		return fmt.Errorf("auto_tls: storage mismatch across sources (%q vs %q)", first.Storage, a.Storage)
	}
	if first.Directory != a.Directory {
		return fmt.Errorf("auto_tls: directory mismatch across sources (%q vs %q)", first.Directory, a.Directory)
	}
	return nil
}
