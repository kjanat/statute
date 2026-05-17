package statute

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"

	"statute.kjanat.dev/resolved"
)

// dns01Manager is a minimal cert manager that satisfies ACME DNS-01
// challenges via Cloudflare's DNS API. It mirrors the public surface of
// autocert.Manager (GetCertificate for tls.Config, HTTPHandler for the plain
// HTTP listener) but takes a totally different path for challenge
// satisfaction.
//
// Storage layout (one directory per manager):
//
//	<storage>/dns01/account.key   — ACME account private key (ECDSA P-256)
//	<storage>/dns01/<host>.crt    — full certificate chain (PEM, leaf first)
//	<storage>/dns01/<host>.key    — certificate private key (PEM, ECDSA P-256)
//
// The manager runs a background renewal goroutine that wakes hourly and
// re-issues any cert whose leaf expires within 30 days. On first start, it
// blocks the first GetCertificate call until the cert is available.
type dns01Manager struct {
	domains []string
	email   string
	storage string
	cf      *cloudflareAPI
	zoneID  string

	mu    sync.RWMutex
	cache map[string]*tls.Certificate

	acmeClient *acme.Client
	accountKey *ecdsa.PrivateKey

	cancel context.CancelFunc
	done   chan struct{}
}

func newDNS01Manager(cfg *resolved.AutoTLS) (*dns01Manager, error) {
	if cfg == nil || cfg.DNS01 == nil {
		return nil, errors.New("dns01: nil config")
	}
	dir := filepath.Join(cfg.Storage, "dns01")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	return &dns01Manager{
		domains: cfg.Domains,
		email:   cfg.Email,
		storage: dir,
		cf:      newCloudflareAPI(cfg.DNS01.APIToken),
		zoneID:  cfg.DNS01.ZoneID,
		cache:   make(map[string]*tls.Certificate),
	}, nil
}

func (m *dns01Manager) start() error {
	if err := m.loadOrCreateAccount(); err != nil {
		return err
	}
	// Issue any missing certs upfront so the first TLS handshake is fast.
	for _, d := range m.domains {
		if _, err := m.getOrIssue(context.Background(), d); err != nil {
			log.Printf("statute: dns01: initial issuance for %s failed: %v", d, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go m.renewalLoop(ctx)
	return nil
}

func (m *dns01Manager) stop() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	<-m.done
}

// GetCertificate satisfies tls.Config.GetCertificate. It looks up the
// certificate for the SNI hostname; if absent it triggers issuance inline.
// A long handshake is preferable to a missing cert.
func (m *dns01Manager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if host == "" {
		return nil, errors.New("dns01: SNI hostname empty")
	}
	if !m.coversHost(host) {
		return nil, fmt.Errorf("dns01: host %q not configured", host)
	}
	return m.getOrIssue(hello.Context(), host)
}

func (m *dns01Manager) coversHost(host string) bool {
	for _, d := range m.domains {
		if strings.EqualFold(d, host) {
			return true
		}
		// wildcard match: *.example.com matches foo.example.com but not bar.foo.example.com
		if strings.HasPrefix(d, "*.") {
			suffix := d[1:]
			if strings.HasSuffix(strings.ToLower(host), strings.ToLower(suffix)) &&
				strings.Count(host, ".") == strings.Count(d, ".") {
				return true
			}
		}
	}
	return false
}

func (m *dns01Manager) getOrIssue(ctx context.Context, host string) (*tls.Certificate, error) {
	m.mu.RLock()
	cert, ok := m.cache[host]
	m.mu.RUnlock()
	if ok && certValid(cert) {
		return cert, nil
	}
	// Try loading from disk before we go to ACME.
	if cert := m.loadCert(host); cert != nil && certValid(cert) {
		m.mu.Lock()
		m.cache[host] = cert
		m.mu.Unlock()
		return cert, nil
	}
	cert, err := m.issue(ctx, host)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.cache[host] = cert
	m.mu.Unlock()
	return cert, nil
}

func certValid(c *tls.Certificate) bool {
	if c == nil || len(c.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return false
	}
	return time.Now().Before(leaf.NotAfter.Add(-30 * 24 * time.Hour))
}

func (m *dns01Manager) renewalLoop(ctx context.Context) {
	defer close(m.done)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, d := range m.domains {
				if cert := m.loadCert(d); cert == nil || !certValid(cert) {
					if _, err := m.issue(ctx, d); err != nil {
						log.Printf("statute: dns01: renewal for %s failed: %v", d, err)
					}
				}
			}
		}
	}
}

func (m *dns01Manager) issue(ctx context.Context, host string) (*tls.Certificate, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	authzID := acme.AuthzID{Type: "dns", Value: host}
	order, err := m.acmeClient.AuthorizeOrder(ctx, []acme.AuthzID{authzID})
	if err != nil {
		return nil, fmt.Errorf("authorize order: %w", err)
	}

	if err := m.solveAuthorizations(ctx, host, order); err != nil {
		return nil, err
	}
	return m.finalizeOrder(ctx, host, order)
}

// solveAuthorizations satisfies the dns-01 challenge for every pending
// authorization in the order. Authorizations already valid are skipped.
func (m *dns01Manager) solveAuthorizations(ctx context.Context, host string, order *acme.Order) error {
	for _, authzURL := range order.AuthzURLs {
		authz, err := m.acmeClient.GetAuthorization(ctx, authzURL)
		if err != nil {
			return fmt.Errorf("get authorization: %w", err)
		}
		if authz.Status == acme.StatusValid {
			continue
		}
		dns01 := findDNS01Challenge(authz.Challenges)
		if dns01 == nil {
			return fmt.Errorf("no dns-01 challenge offered for %s", host)
		}
		if err := m.satisfyDNS01(ctx, host, dns01); err != nil {
			return fmt.Errorf("dns-01: %w", err)
		}
	}
	return nil
}

// findDNS01Challenge returns the dns-01 challenge from an authorization's
// challenge list, or nil if none is offered.
func findDNS01Challenge(challenges []*acme.Challenge) *acme.Challenge {
	for _, ch := range challenges {
		if ch.Type == "dns-01" {
			return ch
		}
	}
	return nil
}

// finalizeOrder generates a key + CSR, finalizes the ACME order, and
// best-effort persists the issued certificate.
func (m *dns01Manager) finalizeOrder(ctx context.Context, host string, order *acme.Order) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("cert key: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: host},
		DNSNames: []string{host},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("csr: %w", err)
	}
	certDER, _, err := m.acmeClient.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}

	cert := &tls.Certificate{Certificate: certDER, PrivateKey: key}
	if err := m.persistCert(host, certDER, key); err != nil {
		log.Printf("statute: dns01: persist for %s failed: %v", host, err)
	}
	return cert, nil
}

func (m *dns01Manager) satisfyDNS01(ctx context.Context, host string, ch *acme.Challenge) error {
	value, err := m.acmeClient.DNS01ChallengeRecord(ch.Token)
	if err != nil {
		return err
	}
	zoneID := m.zoneID
	if zoneID == "" {
		zoneID, err = m.cf.findZoneID(ctx, host)
		if err != nil {
			return err
		}
	}
	recordName := "_acme-challenge." + strings.TrimPrefix(host, "*.")
	recordID, err := m.cf.addTXTRecord(ctx, zoneID, recordName, value)
	if err != nil {
		return fmt.Errorf("add TXT record: %w", err)
	}
	defer func() { //nolint:contextcheck // detached ctx on purpose: best-effort cleanup must run even after the parent ctx is cancelled
		// Cleanup is best-effort. Cloudflare's free-tier 60s TTL means a
		// stale record is harmless after a couple of minutes.
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if derr := m.cf.deleteRecord(dctx, zoneID, recordID); derr != nil {
			log.Printf("statute: dns01: cleanup TXT %s: %v", recordName, derr)
		}
	}()

	// DNS propagation: wait briefly before telling ACME to validate.
	// Cloudflare's authoritative resolvers are fast (~10s); we sleep up to
	// 30s and let ACME's own retries handle the rest.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
	}

	if _, err := m.acmeClient.Accept(ctx, ch); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}
	if _, err := m.acmeClient.WaitAuthorization(ctx, ch.URI); err != nil {
		return fmt.Errorf("wait authorization: %w", err)
	}
	return nil
}

// --- account + cert persistence ---

func (m *dns01Manager) loadOrCreateAccount() error {
	keyPath := filepath.Join(m.storage, "account.key")
	var key *ecdsa.PrivateKey
	if pemBytes, err := os.ReadFile(keyPath); err == nil { //nolint:gosec // G304: fixed filename under the operator-configured storage dir
		k, err := parseECPrivateKey(pemBytes)
		if err != nil {
			return fmt.Errorf("parse account key: %w", err)
		}
		key = k
	} else {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return fmt.Errorf("generate account key: %w", err)
		}
		key = k
		if err := writeECPrivateKey(keyPath, k); err != nil {
			return fmt.Errorf("write account key: %w", err)
		}
	}
	m.accountKey = key
	m.acmeClient = &acme.Client{
		Key:          key,
		DirectoryURL: acme.LetsEncryptURL,
	}
	contact := []string{"mailto:" + m.email}
	if _, err := m.acmeClient.Register(context.Background(), &acme.Account{Contact: contact}, acme.AcceptTOS); err != nil {
		// Already registered is fine. The acme library returns ErrAccountAlreadyExists
		// for that case.
		if !errors.Is(err, acme.ErrAccountAlreadyExists) {
			return fmt.Errorf("acme register: %w", err)
		}
	}
	return nil
}

func (m *dns01Manager) loadCert(host string) *tls.Certificate {
	// host is gated by coversHost against the configured domain allowlist
	// before any call reaches loadCert, so it cannot contain path traversal.
	certPEM, err := os.ReadFile(filepath.Join(m.storage, host+".crt")) //nolint:gosec // G304: see above
	if err != nil {
		return nil
	}
	keyPEM, err := os.ReadFile(filepath.Join(m.storage, host+".key")) //nolint:gosec // G304: host allowlist-gated by coversHost (see loadCert above)
	if err != nil {
		return nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil
	}
	return &cert
}

func (m *dns01Manager) persistCert(host string, chain [][]byte, key *ecdsa.PrivateKey) error {
	// Pre-allocate to roughly the encoded size: PEM adds ~1/3 overhead on
	// base64 plus header/footer bytes. The exact size is unimportant; we just
	// want to avoid the growth-and-copy cycle for typical 3-cert chains.
	certPEM := make([]byte, 0, 4096)
	for _, der := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: der,
		})...)
	}
	if err := os.WriteFile(filepath.Join(m.storage, host+".crt"), certPEM, 0o600); err != nil {
		return err
	}
	return writeECPrivateKey(filepath.Join(m.storage, host+".key"), key)
}

// dns01TLSConfig returns a *tls.Config that pulls certificates from the
// given DNS-01 manager. ALPN includes h2 when HTTP/2 is enabled. Unlike
// autocert, no "acme-tls/1" entry is needed because DNS-01 does not use
// ALPN at all.
func dns01TLSConfig(m *dns01Manager, http2 bool) *tls.Config {
	cfg := &tls.Config{
		GetCertificate: m.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
	if http2 {
		cfg.NextProtos = []string{alpnHTTP2, alpnHTTP1}
	} else {
		cfg.NextProtos = []string{alpnHTTP1}
	}
	return cfg
}

func parseECPrivateKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func writeECPrivateKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	return os.WriteFile(path, pemBytes, 0o600)
}
