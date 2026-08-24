package statute

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
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

const (
	// acmeRenewBefore is how long before expiry a certificate is re-issued.
	// It gates renewal only — a certificate inside this window still
	// terminates handshakes, the same split autocert draws between
	// validCert (serving) and its RenewBefore machinery (renewal).
	acmeRenewBefore = 30 * 24 * time.Hour
	// acmeIssueTimeout caps one order, validation included.
	acmeIssueTimeout = 5 * time.Minute
	// acmeIssueRetryAfter is how long a failed order is remembered so a
	// burst of handshakes for a host the CA rejects hits the cached error
	// instead of the CA. Same value as autocert.createCertRetryAfter.
	acmeIssueRetryAfter = time.Minute
	// acmeDeactivateTimeout caps the detached cleanup of the pending
	// authorizations a failed order leaves behind.
	acmeDeactivateTimeout = 30 * time.Second
	// acmeRegisterTimeout caps account registration: one directory fetch
	// plus one POST. It runs inside Start, under the server mutex, so an
	// unresponsive directory must surface as a failed Start rather than
	// hang the whole lifecycle on a dead CA.
	acmeRegisterTimeout = time.Minute
)

// acmeSolver satisfies one kind of ACME challenge for the manager. The
// manager owns account, order, and certificate handling; the solver owns
// publishing the challenge response and driving validation.
type acmeSolver interface {
	// challengeType names the ACME challenge this solver satisfies
	// ("dns-01", "http-01").
	challengeType() string
	// satisfy publishes the response for one challenge, accepts it, waits
	// for the authorization at authzURL to validate, and cleans up after
	// itself. Waiting must poll the authorization, not the challenge:
	// authorization-level terminal states (expired, deactivated, revoked —
	// RFC 8555 §7.1.6) never appear on a challenge object, and an
	// acme.AuthorizationError raised from a challenge URL carries no
	// identifier.
	satisfy(ctx context.Context, client *acme.Client, host, authzURL string, ch *acme.Challenge) error
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
// A background renewal goroutine wakes hourly and re-issues any cert whose
// leaf expires within acmeRenewBefore; handshakes keep being served from
// the old cert until the replacement lands, and issuance happens inline
// only for a host with no usable certificate at all. Orders are
// deduplicated per host, so concurrent handshakes for one cold name share
// a single order and a renewal never races a handshake.
type acmeManager struct {
	name    string // challenge nickname: storage subdir and log prefix
	domains []string
	email   string
	storage string
	solver  acmeSolver

	// issueTimeout caps one whole order, validation included. It is
	// acmeIssueTimeout plus whatever propagation budget a DNS-01 source
	// declared: a policy allowed to wait ten minutes for its TXT record
	// must not have the order cancelled out from under it at five, which
	// would abandon the authorization and burn the wait for nothing.
	issueTimeout time.Duration

	// directoryURL is the ACME directory. Let's Encrypt in production;
	// tests point it at a local fake.
	directoryURL string

	mu    sync.RWMutex
	cache map[string]*tls.Certificate

	issueMu sync.Mutex
	issuing map[string]*issueState

	acmeClient *acme.Client
	accountKey *ecdsa.PrivateKey

	// lifecycleMu guards the fields below: start, stop, and every reader
	// of runCtx run on different goroutines.
	lifecycleMu sync.Mutex
	started     bool
	runCtx      context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	warmWG      sync.WaitGroup
}

// issueState is one host's in-flight or recently failed ACME order. The
// caller that creates it owns the order and closes ready when it settles;
// every other caller waits on ready and reads the result. Mirrors
// autocert's certState, with the failure kept for acmeIssueRetryAfter.
type issueState struct {
	ready    chan struct{}
	cert     *tls.Certificate
	err      error
	failedAt time.Time
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
		issueTimeout: acmeIssueTimeout + dns01PropagationBudget(cfg),
		directoryURL: acme.LetsEncryptURL,
		cache:        make(map[string]*tls.Certificate),
		issuing:      make(map[string]*issueState),
	}, nil
}

// dns01PropagationBudget is the wall-clock time a source's DNS-01
// propagation policy may spend inside one order: the fixed delay plus the
// polling deadline. It is zero without a policy — the built-in
// dns01PropagationDelay fits inside acmeIssueTimeout with room to spare.
func dns01PropagationBudget(cfg *resolved.AutoTLS) time.Duration {
	if cfg.DNS01 == nil || cfg.DNS01.Propagation == nil {
		return 0
	}
	return cfg.DNS01.Propagation.Delay + cfg.DNS01.Propagation.Timeout
}

// start loads or registers the ACME account and begins the renewal loop.
// It issues nothing: eager issuance is warm's job, because when it may run
// depends on the solver — an HTTP-01 CA validates by fetching the token
// from the plain HTTP listener, which does not serve until the server has
// opened its listeners. Starting an already started manager is an error:
// reassigning the lifecycle fields would orphan the running loop and leave
// its done channel to be closed twice.
func (m *acmeManager) start() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.started {
		return fmt.Errorf("%s: already started", m.name)
	}
	if err := m.loadOrCreateAccount(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.runCtx, m.cancel, m.done, m.started = ctx, cancel, done, true
	go m.renewalLoop(ctx, done)
	return nil
}

// warm issues a certificate for every domain that has none yet, so the
// first TLS handshake is fast. The server runs it synchronously before
// opening listeners for solvers that validate without them (DNS-01), and
// through warmAsync after the listeners serve for HTTP-01 — issuing
// earlier would point the CA at a port nobody answers yet and burn a
// failed validation. Stopping the manager cancels an in-flight warm-up.
func (m *acmeManager) warm() {
	ctx := m.runContext()
	for _, d := range m.domains {
		if ctx.Err() != nil {
			return
		}
		if _, err := m.getOrIssue(ctx, d); err != nil && ctx.Err() == nil {
			log.Printf("statute: %s: initial issuance for %s failed: %v", m.name, d, err)
		}
	}
}

// warmAsync runs warm in a goroutine tracked by the manager, so stop waits
// for it instead of returning while an order is still in flight.
func (m *acmeManager) warmAsync() {
	m.warmWG.Go(m.warm)
}

// warmsAfterListeners reports whether eager issuance must wait until the
// listeners serve: an HTTP-01 CA fetches the token from the plain HTTP
// listener, so warming before Start opens it can only fail.
func (m *acmeManager) warmsAfterListeners() bool {
	_, ok := m.solver.(*http01Solver)
	return ok
}

// stop cancels the manager's run context, waits for the renewal loop to
// exit, and then waits for any warm-up goroutine. Both waits matter at
// shutdown: an unwaited warm can still be talking to the CA — and logging
// — after the process believes it has stopped. Stopping a manager that
// never started, or stopping twice, is a no-op.
func (m *acmeManager) stop() {
	m.lifecycleMu.Lock()
	cancel, done := m.cancel, m.done
	// runCtx is deliberately left in place, now cancelled, so issuance
	// racing the shutdown fails fast instead of falling back to a
	// background context.
	m.cancel, m.done, m.started = nil, nil, false
	m.lifecycleMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	m.warmWG.Wait()
}

// runContext returns the context ACME work runs under. It is the
// manager's own, never a caller's: a client that drops its handshake must
// not cancel a validation the CA is in the middle of. Before start (and
// after a manager that never started) there is none, and tests call
// GetCertificate on an unstarted manager.
func (m *acmeManager) runContext() context.Context {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.runCtx == nil {
		return context.Background()
	}
	return m.runCtx
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
// certificate for the SNI hostname; if none is usable it triggers issuance
// inline. A long handshake is preferable to a missing cert. The name goes
// through the same canonicalTLSName as the router and the resolved domains
// — a U-label ClientHello must match the A-label domain it resolved to.
func (m *acmeManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := canonicalTLSName(hello.ServerName)
	if host == "" {
		return nil, fmt.Errorf("%s: SNI hostname empty", m.name)
	}
	domain, ok := m.matchDomain(host)
	if !ok {
		return nil, fmt.Errorf("%s: host %q not configured", m.name, host)
	}
	ctx := hello.Context()
	if ctx == nil {
		// crypto/tls always sets one; a hand-built ClientHelloInfo does not.
		ctx = context.Background()
	}
	return m.getOrIssue(ctx, domain)
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

// getOrIssue serves any usable certificate for host and orders one only
// when there is none. Renewal freshness is not a serving predicate: a cert
// inside the renewal window still terminates TLS, and treating it as
// absent would put a full ACME order in every handshake for the last 30
// days of its life — five duplicate certificates a week is all Let's
// Encrypt grants for one name set — and would fail the handshake outright
// whenever that order failed.
func (m *acmeManager) getOrIssue(ctx context.Context, host string) (*tls.Certificate, error) {
	if cert := m.usable(host); cert != nil {
		return cert, nil
	}
	return m.issueOnce(ctx, host)
}

// usable returns a servable certificate for host from the memory cache or,
// failing that, from disk — caching what it loads — and nil when neither
// holds one.
func (m *acmeManager) usable(host string) *tls.Certificate {
	m.mu.RLock()
	cert, ok := m.cache[host]
	m.mu.RUnlock()
	if ok && usableCert(cert) {
		return cert
	}
	cert = m.loadCert(host)
	if !usableCert(cert) {
		return nil
	}
	m.mu.Lock()
	m.cache[host] = cert
	m.mu.Unlock()
	return cert
}

// issueOnce runs at most one ACME order per host at a time. The owning
// caller runs the order under the manager's context so an abandoned
// handshake cannot cancel a validation mid-flight — yanking the published
// token away from the CA's fetch burns one of the five validation failures
// Let's Encrypt allows per hostname per hour. Later callers wait on the
// entry but honour their own context while waiting. Mirrors
// autocert.Manager.createCert over its certState table.
func (m *acmeManager) issueOnce(ctx context.Context, host string) (*tls.Certificate, error) {
	st, owner := m.issueState(host)
	if !owner {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-st.ready:
			return st.cert, st.err
		}
	}
	timeout := m.issueTimeout
	if timeout <= 0 {
		// Every production manager gets issueTimeout from newACMEManager;
		// zero means a hand-built test literal, and an already-expired
		// order context would be a miserable way to find that out.
		timeout = acmeIssueTimeout
	}
	parent := m.runContext()
	ictx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	//nolint:contextcheck // detached from the caller on purpose: see this function's doc comment
	st.cert, st.err = m.issue(ictx, host)
	if st.err != nil {
		if errors.Is(st.err, context.Canceled) && parent.Err() != nil {
			// The manager was stopped and the order died of that
			// cancellation: lifecycle, not a CA verdict. Drop the state
			// before ready closes, so no later caller can find the
			// settled cancellation through the table — a restarted
			// manager issues immediately instead of replaying it for the
			// cooldown window. Both conditions matter: a genuine CA
			// failure that lands just as stop cancels the context is not
			// itself a cancellation and keeps its cooldown.
			m.forgetIssueState(host, st)
		} else {
			// A CA verdict (or a timeout under a live manager) is kept
			// for acmeIssueRetryAfter; issueState drops it once the
			// cooldown elapses.
			st.failedAt = time.Now()
		}
		close(st.ready)
		return nil, st.err
	}
	m.mu.Lock()
	m.cache[host] = st.cert
	m.mu.Unlock()
	close(st.ready)
	m.forgetIssueState(host, st)
	return st.cert, nil
}

// issueState returns host's issuance entry and whether the caller owns it.
// A settled failure is handed out until its cooldown expires, after which
// the next caller owns a fresh entry.
func (m *acmeManager) issueState(host string) (*issueState, bool) {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	if m.issuing == nil {
		m.issuing = make(map[string]*issueState)
	}
	if st, ok := m.issuing[host]; ok && !st.stale(time.Now()) {
		return st, false
	}
	st := &issueState{ready: make(chan struct{})}
	m.issuing[host] = st
	return st, true
}

// forgetIssueState drops st from the table if it is still the current
// entry, so the next order for host (a renewal, or a handshake after the
// cached cert expires) starts fresh.
func (m *acmeManager) forgetIssueState(host string, st *issueState) {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	if m.issuing[host] == st {
		delete(m.issuing, host)
	}
}

// stale reports whether the entry may be replaced. An order still in
// flight never is; a settled one is stale once its cooldown has passed. A
// success is stale immediately — its owner removes it — so a leftover
// cannot pin a certificate that later expires.
func (st *issueState) stale(now time.Time) bool {
	select {
	case <-st.ready:
	default:
		return false
	}
	return st.err == nil || now.After(st.failedAt.Add(acmeIssueRetryAfter))
}

// usableCert reports whether c can terminate a handshake right now: it
// parses and now falls inside the leaf's validity window. Compare
// autocert.validCert, which likewise gates serving on nothing but the
// leaf's own NotBefore/NotAfter.
func usableCert(c *tls.Certificate) bool {
	leaf := certLeaf(c)
	if leaf == nil {
		return false
	}
	now := time.Now()
	return !now.Before(leaf.NotBefore) && !now.After(leaf.NotAfter)
}

// needsRenewal reports whether c must be re-issued: missing, unparseable,
// or inside the acmeRenewBefore window before expiry. It never withholds a
// certificate from a handshake — that is usableCert's call.
func needsRenewal(c *tls.Certificate) bool {
	leaf := certLeaf(c)
	if leaf == nil {
		return true
	}
	return !time.Now().Before(leaf.NotAfter.Add(-acmeRenewBefore))
}

func certLeaf(c *tls.Certificate) *x509.Certificate {
	if c == nil || len(c.Certificate) == 0 {
		return nil
	}
	if c.Leaf != nil {
		return c.Leaf
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// renewalLoop re-issues expiring certificates until ctx is cancelled. done
// is a parameter, not the manager field, so a loop started by an earlier
// start always closes the channel that start's stop is waiting on.
func (m *acmeManager) renewalLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.renewExpiring(ctx)
		}
	}
}

// renewExpiring re-issues every domain whose certificate is missing or
// inside the renewal window. It goes through issueOnce so a renewal and a
// handshake can never order for the same host at once, and so the fresh
// certificate reaches the in-memory cache — reloading it from disk would
// keep serving the old one whenever persistence failed.
func (m *acmeManager) renewExpiring(ctx context.Context) {
	for _, d := range m.domains {
		if !needsRenewal(m.usable(d)) {
			continue
		}
		if _, err := m.issueOnce(ctx, d); err != nil && ctx.Err() == nil {
			log.Printf("statute: %s: renewal for %s failed: %v", m.name, d, err)
		}
	}
}

func (m *acmeManager) issue(ctx context.Context, host string) (*tls.Certificate, error) {
	authzID := acme.AuthzID{Type: "dns", Value: host}
	order, err := m.acmeClient.AuthorizeOrder(ctx, []acme.AuthzID{authzID})
	if err != nil {
		return nil, fmt.Errorf("authorize order: %w", err)
	}
	cert, err := m.solveAndFinalize(ctx, host, order)
	if err != nil {
		// Remove all hanging authorizations to reduce rate limit quotas
		// (autocert.Manager.deactivatePendingAuthz). An order abandoned
		// before or during validation leaves its authorizations pending
		// against the account's 300-pending limit for seven days.
		//nolint:contextcheck,gosec // G118: detached by design — the issuance context is dead exactly when this cleanup is needed
		go m.deactivatePendingAuthz(order.AuthzURLs)
		return nil, err
	}
	return cert, nil
}

func (m *acmeManager) solveAndFinalize(ctx context.Context, host string, order *acme.Order) (*tls.Certificate, error) {
	if err := m.solveAuthorizations(ctx, host, order); err != nil {
		return nil, err
	}
	return m.finalizeOrder(ctx, host, order)
}

// deactivatePendingAuthz revokes the still-pending authorizations of an
// abandoned order. It runs on its own context because the issuance context
// is dead by the time it is called — a cancelled validation is exactly the
// case it cleans up after — which is why autocert's counterpart is
// likewise detached.
func (m *acmeManager) deactivatePendingAuthz(urls []string) {
	ctx, cancel := context.WithTimeout(context.Background(), acmeDeactivateTimeout)
	defer cancel()
	for _, u := range urls {
		authz, err := m.acmeClient.GetAuthorization(ctx, u)
		if err != nil || authz.Status != acme.StatusPending {
			continue
		}
		if err := m.acmeClient.RevokeAuthorization(ctx, u); err != nil {
			log.Printf("statute: %s: deactivating pending authorization: %v", m.name, err)
		}
	}
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
		if err := m.solver.satisfy(ctx, m.acmeClient, host, authzURL, ch); err != nil {
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
	// SAN-only, no Subject: RFC 8555 §7.4 identifies the order by its
	// dNSName SANs, Let's Encrypt stopped issuing a CN in 2024, and Boulder
	// rejects a CSR whose CN exceeds 64 bytes — which every legal DNS name
	// longer than that would hit, at finalize, after validation had already
	// succeeded.
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
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

// loadOrCreateAccount builds the manager's ACME client on the first
// successful start and keeps it for the manager's lifetime. A restarted
// manager reuses it: the account key on disk and the directory URL cannot
// have changed, and a detached authorization cleanup from an order the
// stop cancelled may still be reading the field — reassigning it would
// race that read. The client is published only after Register succeeds,
// so a start that failed at registration retries the whole sequence.
func (m *acmeManager) loadOrCreateAccount() error {
	if m.acmeClient != nil {
		return nil
	}
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
	client := &acme.Client{
		Key:          key,
		DirectoryURL: m.directoryURL,
	}
	contact := []string{"mailto:" + m.email}
	ctx, cancel := context.WithTimeout(context.Background(), acmeRegisterTimeout)
	defer cancel()
	if _, err := client.Register(ctx, &acme.Account{Contact: contact}, acme.AcceptTOS); err != nil {
		// Already registered is fine. The acme library returns ErrAccountAlreadyExists
		// for that case.
		if !errors.Is(err, acme.ErrAccountAlreadyExists) {
			return fmt.Errorf("acme register: %w", err)
		}
	}
	m.accountKey = key
	m.acmeClient = client
	return nil
}

func (m *acmeManager) loadCert(host string) *tls.Certificate {
	// host is gated by coversHost against the configured domain allowlist
	// before any call reaches loadCert, so it cannot contain path traversal.
	certPEM, err := os.ReadFile(filepath.Join(m.storage, host+".crt")) //nolint:gosec // G304: see above
	if err != nil {
		// No chain yet is the normal cold-start state; anything else is a
		// storage problem the operator needs to see.
		if !os.IsNotExist(err) {
			log.Printf("statute: %s: reading stored chain for %s: %v", m.name, host, err)
		}
		return nil
	}
	keyPEM, err := os.ReadFile(filepath.Join(m.storage, host+".key")) //nolint:gosec // G304: host allowlist-gated by coversHost (see loadCert above)
	if err != nil {
		// The chain is on disk, so its key must be as well: persistCert
		// renames both into place. Missing or unreadable here means the
		// pair was damaged from outside.
		log.Printf("statute: %s: stored chain for %s has no readable key: %v", m.name, host, err)
		return nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Printf("statute: %s: stored certificate for %s is unusable: %v", m.name, host, err)
		return nil
	}
	return &cert
}

// persistCert writes the chain and the key through temp files in the same
// directory and renames them into place, because loadCert reads them as a
// pair: two plain O_TRUNC writes leave a window — and a crash or a second
// writer leaves it permanently — in which the chain and key on disk do not
// match, which tls.X509KeyPair rejects. autocert sidesteps this by keeping
// key and chain in a single cache entry; we keep the operator-readable
// two-file layout and order the renames so the chain, the file loadCert
// opens first, is the last to change.
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
	keyPEM, err := encodeECPrivateKey(key)
	if err != nil {
		return err
	}
	certTmp, err := writeTempFile(m.storage, host+".crt", certPEM)
	if err != nil {
		return err
	}
	keyTmp, err := writeTempFile(m.storage, host+".key", keyPEM)
	if err != nil {
		_ = os.Remove(certTmp)
		return err
	}
	if err := os.Rename(keyTmp, filepath.Join(m.storage, host+".key")); err != nil {
		_ = os.Remove(certTmp)
		_ = os.Remove(keyTmp)
		return err
	}
	if err := os.Rename(certTmp, filepath.Join(m.storage, host+".crt")); err != nil {
		_ = os.Remove(certTmp)
		return err
	}
	return nil
}

// writeTempFile writes data to a fresh 0600 file in dir and returns its
// path for the caller to rename into place or remove. The temp file shares
// dir so the rename stays within one filesystem, hence atomic.
func writeTempFile(dir, prefix string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, prefix+".tmp*")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	// Sync before the rename: without it a crash can publish the name while
	// the bytes behind it are still in page cache.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func parseECPrivateKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func encodeECPrivateKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func writeECPrivateKey(path string, key *ecdsa.PrivateKey) error {
	pemBytes, err := encodeECPrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pemBytes, 0o600)
}
