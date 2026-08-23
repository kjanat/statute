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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"

	"statute.kjanat.dev/resolved"
)

// acmeSolver satisfies one kind of ACME challenge for the manager. The
// manager owns account, order, and certificate handling; the solver owns
// publishing the challenge response and driving validation.
type acmeSolver interface {
	// challengeType names the ACME challenge this solver satisfies
	// ("dns-01", "http-01").
	challengeType() string
	// satisfy publishes the response for one challenge, accepts it, waits
	// for the authorization to validate, and cleans up after itself.
	satisfy(ctx context.Context, client *acme.Client, host string, ch *acme.Challenge) error
}

// acmeManager is a minimal cert manager that issues certificates over a
// single ACME challenge type, delegated to its solver. It mirrors the
// public surface of autocert.Manager (GetCertificate for tls.Config) but —
// unlike autocert, whose challenge preference is hard-coded to try
// TLS-ALPN-01 first — it only ever attempts the solver's challenge, so a
// source pinned to HTTP-01 or DNS-01 never burns a failed validation on a
// challenge type it was told not to use.
//
// Storage layout (one directory per manager, named for the challenge):
//
//	<storage>/<name>/account.key   — ACME account private key (ECDSA P-256)
//	<storage>/<name>/<host>.crt    — full certificate chain (PEM, leaf first)
//	<storage>/<name>/<host>.key    — certificate private key (PEM, ECDSA P-256)
//
// The manager runs a background renewal goroutine that wakes hourly and
// re-issues any cert whose leaf expires within 30 days. On first start, it
// blocks the first GetCertificate call until the cert is available.
type acmeManager struct {
	name    string // challenge nickname: storage subdir and log prefix
	domains []string
	email   string
	storage string
	solver  acmeSolver

	// directoryURL is the ACME directory. Let's Encrypt in production;
	// tests point it at a local fake.
	directoryURL string

	mu    sync.RWMutex
	cache map[string]*tls.Certificate

	acmeClient *acme.Client
	accountKey *ecdsa.PrivateKey

	cancel context.CancelFunc
	done   chan struct{}
}

// newACMEManager builds a manager rooted at <cfg.Storage>/<name> issuing
// for cfg's domains through the given solver.
func newACMEManager(cfg *resolved.AutoTLS, name string, solver acmeSolver) (*acmeManager, error) {
	dir := filepath.Join(cfg.Storage, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	return &acmeManager{
		name:         name,
		domains:      cfg.Domains,
		email:        cfg.Email,
		storage:      dir,
		solver:       solver,
		directoryURL: acme.LetsEncryptURL,
		cache:        make(map[string]*tls.Certificate),
	}, nil
}

func (m *acmeManager) start() error {
	if err := m.loadOrCreateAccount(); err != nil {
		return err
	}
	// Issue any missing certs upfront so the first TLS handshake is fast.
	for _, d := range m.domains {
		if _, err := m.getOrIssue(context.Background(), d); err != nil {
			log.Printf("statute: %s: initial issuance for %s failed: %v", m.name, d, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go m.renewalLoop(ctx)
	return nil
}

func (m *acmeManager) stop() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	<-m.done
}

// wrapHTTPChallenges serves this manager's pending HTTP-01 challenge
// responses in front of next on a plain HTTP listener. Managers whose
// solver does not answer over HTTP pass the handler through untouched.
func (m *acmeManager) wrapHTTPChallenges(next http.Handler) http.Handler {
	if s, ok := m.solver.(*http01Solver); ok {
		return s.wrap(next)
	}
	return next
}

// GetCertificate satisfies tls.Config.GetCertificate. It looks up the
// certificate for the SNI hostname; if absent it triggers issuance inline.
// A long handshake is preferable to a missing cert.
func (m *acmeManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if host == "" {
		return nil, fmt.Errorf("%s: SNI hostname empty", m.name)
	}
	domain, ok := m.matchDomain(host)
	if !ok {
		return nil, fmt.Errorf("%s: host %q not configured", m.name, host)
	}
	return m.getOrIssue(hello.Context(), domain)
}

func (m *acmeManager) coversHost(host string) bool {
	_, ok := m.matchDomain(host)
	return ok
}

func (m *acmeManager) matchDomain(host string) (string, bool) {
	// Prefer an exact certificate when both an exact name and a wildcard
	// cover the SNI host, regardless of their configuration order.
	for _, d := range m.domains {
		if strings.EqualFold(d, host) {
			return d, true
		}
	}
	for _, d := range m.domains {
		// wildcard match: *.example.com matches foo.example.com but not bar.foo.example.com
		if strings.HasPrefix(d, "*.") {
			suffix := d[1:]
			if strings.HasSuffix(strings.ToLower(host), strings.ToLower(suffix)) &&
				strings.Count(host, ".") == strings.Count(d, ".") {
				return d, true
			}
		}
	}
	return "", false
}

func (m *acmeManager) getOrIssue(ctx context.Context, host string) (*tls.Certificate, error) {
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

func (m *acmeManager) renewalLoop(ctx context.Context) {
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
						log.Printf("statute: %s: renewal for %s failed: %v", m.name, d, err)
					}
				}
			}
		}
	}
}

func (m *acmeManager) issue(ctx context.Context, host string) (*tls.Certificate, error) {
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

// solveAuthorizations satisfies the solver's challenge for every pending
// authorization in the order. Authorizations already valid are skipped.
func (m *acmeManager) solveAuthorizations(ctx context.Context, host string, order *acme.Order) error {
	typ := m.solver.challengeType()
	for _, authzURL := range order.AuthzURLs {
		authz, err := m.acmeClient.GetAuthorization(ctx, authzURL)
		if err != nil {
			return fmt.Errorf("get authorization: %w", err)
		}
		if authz.Status == acme.StatusValid {
			continue
		}
		ch := findChallenge(typ, authz.Challenges)
		if ch == nil {
			return fmt.Errorf("no %s challenge offered for %s", typ, host)
		}
		if err := m.solver.satisfy(ctx, m.acmeClient, host, ch); err != nil {
			return fmt.Errorf("%s: %w", typ, err)
		}
	}
	return nil
}

// findChallenge returns the challenge of the given type from an
// authorization's challenge list, or nil if none is offered.
func findChallenge(typ string, challenges []*acme.Challenge) *acme.Challenge {
	for _, ch := range challenges {
		if ch.Type == typ {
			return ch
		}
	}
	return nil
}

// finalizeOrder generates a key + CSR, finalizes the ACME order, and
// best-effort persists the issued certificate.
func (m *acmeManager) finalizeOrder(ctx context.Context, host string, order *acme.Order) (*tls.Certificate, error) {
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
		log.Printf("statute: %s: persist for %s failed: %v", m.name, host, err)
	}
	return cert, nil
}

// --- account + cert persistence ---

func (m *acmeManager) loadOrCreateAccount() error {
	keyPath := filepath.Join(m.storage, "account.key")
	var key *ecdsa.PrivateKey
	pemBytes, err := os.ReadFile(keyPath) //nolint:gosec // G304: fixed filename under the operator-configured storage dir
	switch {
	case err == nil:
		k, err := parseECPrivateKey(pemBytes)
		if err != nil {
			return fmt.Errorf("parse account key: %w", err)
		}
		key = k
	case os.IsNotExist(err):
		// No account yet — mint one and persist it.
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return fmt.Errorf("generate account key: %w", err)
		}
		key = k
		if err := writeECPrivateKey(keyPath, k); err != nil {
			return fmt.Errorf("write account key: %w", err)
		}
	default:
		// An existing key we cannot read (permissions, I/O). Do NOT mint a
		// new one — that would desync the ACME account. Surface the error.
		return fmt.Errorf("read account key: %w", err)
	}
	m.accountKey = key
	m.acmeClient = &acme.Client{
		Key:          key,
		DirectoryURL: m.directoryURL,
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

func (m *acmeManager) loadCert(host string) *tls.Certificate {
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

func (m *acmeManager) persistCert(host string, chain [][]byte, key *ecdsa.PrivateKey) error {
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
