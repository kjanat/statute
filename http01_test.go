package statute

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"statute.kjanat.dev/resolved"
)

// fakeACME is a minimal RFC 8555 directory server: one account, one order,
// one authorization. It offers BOTH tls-alpn-01 and http-01 challenges and
// records which one the client accepts — the pinned manager must only ever
// touch http-01. On accept it validates the token end-to-end against the
// challenge handler under test.
type fakeACME struct {
	t         *testing.T
	srv       *httptest.Server
	caKey     *ecdsa.PrivateKey
	caCert    *x509.Certificate
	challenge http.Handler // where the CA fetches /.well-known/acme-challenge/*
	// challengeURL, when set, validates over real TCP against a listening
	// server instead of the in-process handler — nil challenge then.
	challengeURL string

	mu          sync.Mutex
	authzValid  bool
	authzFailed bool
	accepted    []string       // challenge types the client accepted
	certPEM     []byte         // issued chain, filled by signCSR
	requests    map[string]int // path -> number of requests served
	deactivated []string       // authz URLs the client gave up
	csrCN       string         // Subject.CommonName of the finalized CSR
	csrNames    []string       // dNSName SANs of the finalized CSR
	// rejectValidation makes the CA fail validation without attempting a
	// token fetch, the way a real CA records a name it could not reach. It
	// is not a fixture error, so unlike a failed fetch it stays off t.
	rejectValidation bool
}

const fakeACMEToken = "tok-fake-acme" //nolint:gosec // G101: fixture challenge token, not a credential

func newFakeACME(t *testing.T, challenge http.Handler) *fakeACME {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake ACME CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeACME{t: t, caKey: caKey, caCert: caCert, challenge: challenge, requests: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeACME) url(path string) string { return f.srv.URL + path }

// jwsPayload extracts the base64url JWS payload of a POST; nil means
// POST-as-GET.
func (f *fakeACME) jwsPayload(r *http.Request) []byte {
	var body struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("fake acme: decode JWS: %v", err)
		return nil
	}
	if body.Payload == "" {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(body.Payload)
	if err != nil {
		f.t.Errorf("fake acme: decode payload: %v", err)
	}
	return b
}

func (f *fakeACME) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeACME) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Replay-Nonce", "nonce-1")
	f.mu.Lock()
	f.requests[r.URL.Path]++
	f.mu.Unlock()
	switch r.URL.Path {
	case "/dir":
		f.writeJSON(w, http.StatusOK, map[string]string{
			"newNonce":   f.url("/nonce"),
			"newAccount": f.url("/new-account"),
			"newOrder":   f.url("/new-order"),
			"revokeCert": f.url("/revoke"),
			"keyChange":  f.url("/key-change"),
		})
	case "/nonce":
		w.WriteHeader(http.StatusOK)
	case "/new-account":
		w.Header().Set("Location", f.url("/account/1"))
		f.writeJSON(w, http.StatusCreated, map[string]any{"status": "valid"})
	case "/new-order":
		var order struct {
			Identifiers []struct{ Type, Value string } `json:"identifiers"`
		}
		_ = json.Unmarshal(f.jwsPayload(r), &order)
		w.Header().Set("Location", f.url("/order/1"))
		f.writeJSON(w, http.StatusCreated, map[string]any{
			"status":         "pending",
			"identifiers":    order.Identifiers,
			"authorizations": []string{f.url("/authz/1")},
			"finalize":       f.url("/finalize/1"),
		})
	case "/authz/1":
		f.handleAuthz(w, r)
	case "/chal/http", "/chal/alpn":
		f.handleChallenge(w, r)
	case "/finalize/1":
		f.handleFinalize(w, r)
	case "/cert/1":
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.certPEM)
	default:
		f.t.Errorf("fake acme: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleAuthz answers the authorization endpoint. A POST carrying
// {"status":"deactivated"} is RevokeAuthorization relinquishing a pending
// authorization; anything else (a POST-as-GET) polls the status.
func (f *fakeACME) handleAuthz(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
	}
	if payload := f.jwsPayload(r); payload != nil {
		_ = json.Unmarshal(payload, &req)
	}
	if req.Status == "deactivated" {
		f.mu.Lock()
		f.deactivated = append(f.deactivated, f.url(r.URL.Path))
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, map[string]any{"status": "deactivated"})
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{
		"status":     f.authzStatus(),
		"identifier": map[string]string{"type": "dns", "value": "pin.example"},
		"challenges": []map[string]string{
			{"type": "tls-alpn-01", "url": f.url("/chal/alpn"), "token": fakeACMEToken, "status": "pending"},
			{"type": "http-01", "url": f.url("/chal/http"), "token": fakeACMEToken, "status": "pending"},
		},
	})
}

// finalizedCSR returns the Subject CommonName and the dNSName SANs of the
// CSR the client sent to finalize.
func (f *fakeACME) finalizedCSR() (string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.csrCN, append([]string(nil), f.csrNames...)
}

// count reports how many requests the fake served for one path.
func (f *fakeACME) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

// deactivations returns the authorization URLs the client relinquished.
func (f *fakeACME) deactivations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deactivated...)
}

// authzStatus is the state the fake CA reports on both the authorization
// and the challenge endpoint: pending until validation runs, then valid —
// or invalid once a validation attempt failed, which is what a real CA
// records when the token fetch never lands. Reporting the failure (rather
// than staying pending forever) is what lets WaitAuthorization return
// instead of polling out the client's five-minute issuance budget.
func (f *fakeACME) authzStatus() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.authzValid:
		return "valid"
	case f.authzFailed:
		return "invalid"
	default:
		return "pending"
	}
}

// handleChallenge answers the challenge endpoints. A non-empty JWS
// payload ({}) is Accept, on which the fake CA validates; a POST-as-GET is
// a status poll, which a client waiting on the authorization — the object
// RFC 8555 §7.5.1 gives a status of its own — never sends.
func (f *fakeACME) handleChallenge(w http.ResponseWriter, r *http.Request) {
	typ := "http-01"
	if r.URL.Path == "/chal/alpn" {
		typ = "tls-alpn-01"
	}
	if f.jwsPayload(r) == nil {
		f.writeJSON(w, http.StatusOK, map[string]string{
			"type": typ, "url": f.url(r.URL.Path), "token": fakeACMEToken, "status": f.authzStatus(),
		})
		return
	}
	f.mu.Lock()
	f.accepted = append(f.accepted, typ)
	f.mu.Unlock()
	if typ == "http-01" {
		f.validateHTTP01()
	}
	f.writeJSON(w, http.StatusOK, map[string]string{
		"type": typ, "url": f.url(r.URL.Path), "token": fakeACMEToken, "status": "processing",
	})
}

// handleFinalize signs the order's CSR with the fake CA.
func (f *fakeACME) handleFinalize(w http.ResponseWriter, r *http.Request) {
	var fin struct {
		CSR string `json:"csr"`
	}
	_ = json.Unmarshal(f.jwsPayload(r), &fin)
	der, err := base64.RawURLEncoding.DecodeString(fin.CSR)
	if err != nil {
		f.t.Errorf("fake acme: decode CSR: %v", err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		f.t.Errorf("fake acme: parse CSR: %v", err)
	}
	f.mu.Lock()
	f.csrCN = csr.Subject.CommonName
	f.csrNames = append([]string(nil), csr.DNSNames...)
	f.mu.Unlock()
	f.signCSR(csr)
	f.writeJSON(w, http.StatusOK, map[string]any{
		"status":      "valid",
		"finalize":    f.url("/finalize/1"),
		"certificate": f.url("/cert/1"),
	})
}

// validateHTTP01 plays the CA's validation role: fetch the key
// authorization from the challenge handler under test — or, when
// challengeURL is set, over real TCP so the listener must actually be
// serving, exactly like a public CA.
func (f *fakeACME) validateHTTP01() {
	f.mu.Lock()
	reject := f.rejectValidation
	f.mu.Unlock()
	if reject {
		f.failAuthz()
		return
	}
	path := "/.well-known/acme-challenge/" + fakeACMEToken
	var code int
	var body string
	if f.challengeURL != "" {
		resp, err := http.Get(f.challengeURL + path)
		if err != nil {
			f.t.Errorf("fake acme: http-01 fetch: %v", err)
			f.failAuthz()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		code, body = resp.StatusCode, string(b)
	} else {
		rec := httptest.NewRecorder()
		f.challenge.ServeHTTP(rec, httptest.NewRequest("GET", "http://pin.example"+path, nil))
		code, body = rec.Code, rec.Body.String()
	}
	if code != http.StatusOK || !strings.HasPrefix(body, fakeACMEToken+".") {
		f.t.Errorf("fake acme: http-01 validation failed: code=%d body=%q", code, body)
		f.failAuthz()
		return
	}
	f.mu.Lock()
	f.authzValid = true
	f.mu.Unlock()
}

// failAuthz records a failed validation attempt so the waiting client sees
// an invalid authorization rather than an authorization that never moves.
func (f *fakeACME) failAuthz() {
	f.mu.Lock()
	f.authzFailed = true
	f.mu.Unlock()
}

func (f *fakeACME) signCSR(csr *x509.CertificateRequest) {
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      csr.Subject,
		DNSNames:     csr.DNSNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, f.caCert, csr.PublicKey, f.caKey)
	if err != nil {
		f.t.Errorf("fake acme: sign: %v", err)
		return
	}
	f.mu.Lock()
	f.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	f.mu.Unlock()
}

// TestHTTP01ManagerIssuesEndToEnd — the pinned manager completes a full
// ACME issuance against a directory that offers BOTH tls-alpn-01 and
// http-01: it must accept only http-01 (autocert's hard-coded preference
// would have taken tls-alpn-01 first), serve the token through the plain
// HTTP listener's handler chain, and come out of warm() with a routable
// certificate that a fresh manager then reloads from disk without touching
// ACME again. Listener lifecycle ordering is TestHTTP01WarmsAfterListeners'
// job — here the challenge handler is wired directly.
func TestHTTP01ManagerIssuesEndToEnd(t *testing.T) {
	t.Parallel()
	storage := t.TempDir()
	src := &resolved.AutoTLS{
		Domains:   []string{"pin.example"},
		Email:     "ops@pin.example",
		Storage:   storage,
		Challenge: resolved.ChallengeHTTP01,
	}
	m, err := newHTTP01Manager(src)
	if err != nil {
		t.Fatalf("newHTTP01Manager: %v", err)
	}
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	fake := newFakeACME(t, m.wrapHTTPChallenges(notFound))
	m.directoryURL = fake.url("/dir")

	if err := m.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.stop()
	m.warm()

	cert, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "pin.example"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(leaf.DNSNames, "pin.example") {
		t.Errorf("issued cert covers %v", leaf.DNSNames)
	}

	fake.mu.Lock()
	accepted := append([]string(nil), fake.accepted...)
	fake.mu.Unlock()
	if len(accepted) != 1 || accepted[0] != "http-01" {
		t.Errorf("challenges accepted: %v, want exactly [http-01]", accepted)
	}

	// The CSR is SAN-only: RFC 8555 §7.4 identifies the order by its
	// dNSName SANs, and Boulder rejects a CommonName over 64 bytes — a
	// length a legal DNS name can exceed, which would fail at finalize
	// after validation had already succeeded.
	cn, names := fake.finalizedCSR()
	if cn != "" {
		t.Errorf("CSR carries Subject.CommonName %q, want none", cn)
	}
	if !slices.Equal(names, []string{"pin.example"}) {
		t.Errorf("CSR DNSNames = %v, want [pin.example]", names)
	}

	assertReloadsFromDisk(t, fake, src, cert)
}

// assertReloadsFromDisk proves persistence: a restarted manager over the
// same storage loads the existing ACME account key and serves the issued
// certificate from disk without ordering again.
func assertReloadsFromDisk(t *testing.T, fake *fakeACME, src *resolved.AutoTLS, issued *tls.Certificate) {
	t.Helper()
	m, err := newHTTP01Manager(src)
	if err != nil {
		t.Fatal(err)
	}
	m.directoryURL = fake.url("/dir")
	if err := m.start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer m.stop()
	cert, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "pin.example"})
	if err != nil {
		t.Fatalf("GetCertificate from disk: %v", err)
	}
	if fmt.Sprintf("%x", cert.Certificate[0]) != fmt.Sprintf("%x", issued.Certificate[0]) {
		t.Error("reloaded certificate differs from the issued one")
	}
}

// TestHTTP01WarmsAfterListeners is the lifecycle regression test for the
// ordering of eager HTTP-01 issuance inside server.Start(). The CA proves
// control of the name by fetching the token over the network, so the plain
// HTTP listener must already be accepting connections before warm-up
// orders a certificate. This test drives the real server: the fake CA
// validates with a live http.Get against the reserved port, which the
// listener only answers once Start has served it. With issuance ordered
// before net.Listen — the shape this replaces — that fetch would hit a
// dead port, the authorization would fail, and the certificate below could
// never appear.
func TestHTTP01WarmsAfterListeners(t *testing.T) {
	httpAddr := reserveAddr(t)
	src, srv := newHTTP01LifecycleServer(t, httpAddr)
	mgr := srv.acmeManagers[src]
	if mgr == nil {
		t.Fatal("no acme manager built for the HTTP-01 source")
	}

	// Point the manager at the fake CA, and the fake CA back at the plain
	// HTTP listener's future address, before anything starts.
	fake := newFakeACME(t, nil)
	fake.challengeURL = "http://" + httpAddr
	mgr.directoryURL = fake.url("/dir")

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()

	waitForCachedCert(t, mgr, "pin.example", 10*time.Second)

	fake.mu.Lock()
	accepted := append([]string(nil), fake.accepted...)
	fake.mu.Unlock()
	if len(accepted) != 1 || accepted[0] != "http-01" {
		t.Errorf("challenges accepted: %v, want exactly [http-01]", accepted)
	}
}

// newHTTP01LifecycleServer builds a server with a plain HTTP listener on
// httpAddr and an HTTPS listener whose single AutoTLS source is pinned to
// HTTP-01, and returns that resolved source alongside the server so the
// caller can reach the manager keyed by it.
func newHTTP01LifecycleServer(t *testing.T, httpAddr string) (*resolved.AutoTLS, *server) {
	t.Helper()
	cfg := Config{
		Listeners: Listeners{
			HTTP(httpAddr),
			HTTPS("127.0.0.1:0", AutoTLS("pin.example").
				HTTP01().
				Email("ops@pin.example").
				Storage(t.TempDir())),
		},
		Upstreams: Upstreams{"a": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}}},
		Routes:    Routes{Match("/*").ProxyTo("a")},
		Shutdown:  Shutdown{GracePeriod: "2s"},
	}
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	for _, l := range r.Listeners {
		if len(l.AutoTLSSources) > 0 {
			return l.AutoTLSSources[0], srv
		}
	}
	t.Fatal("resolved config has no AutoTLS source")
	return nil, nil
}

// reserveAddr binds an ephemeral port, closes it, and returns the address
// so a listener started later can claim it. The race between close and
// re-bind is fine for a hermetic test, and it is the only way to tell the
// fake CA where to fetch the token before the server exists.
func reserveAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// waitForCachedCert polls the manager's certificate cache until host is
// present or the deadline passes. Warm-up runs in the background, so the
// certificate appears asynchronously after Start returns.
func waitForCachedCert(t *testing.T, m *acmeManager, host string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		_, ok := m.cache[host]
		m.mu.RUnlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no certificate cached for %s within %s", host, within)
}

// newPinnedManager builds an HTTP-01 manager over fresh storage, wired to
// a fake CA that validates against the manager's own challenge handler.
func newPinnedManager(t *testing.T) (*acmeManager, *fakeACME) {
	t.Helper()
	m, err := newHTTP01Manager(&resolved.AutoTLS{
		Domains:   []string{"pin.example"},
		Email:     "ops@pin.example",
		Storage:   t.TempDir(),
		Challenge: resolved.ChallengeHTTP01,
	})
	if err != nil {
		t.Fatalf("newHTTP01Manager: %v", err)
	}
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	fake := newFakeACME(t, m.wrapHTTPChallenges(notFound))
	m.directoryURL = fake.url("/dir")
	return m, fake
}

// TestHTTP01_ConcurrentHandshakesShareOneOrder pins the per-host
// deduplication. Every handshake for a name with no certificate reaches
// issuance, and background warm-up means a cold first boot serves those
// handshakes while its own order is still running — without dedup each one
// posts its own new-order, and five duplicate certificates per week for
// one name set is the whole Let's Encrypt budget.
func TestHTTP01_ConcurrentHandshakesShareOneOrder(t *testing.T) {
	t.Parallel()
	m, fake := newPinnedManager(t)
	if err := m.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.stop()

	const callers = 4
	certs := make([]*tls.Certificate, callers)
	errs := make([]error, callers)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			<-release
			certs[i], errs[i] = m.GetCertificate(&tls.ClientHelloInfo{ServerName: "pin.example"})
		})
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: GetCertificate: %v", i, err)
		}
		if certs[i] != certs[0] {
			t.Errorf("caller %d got a different certificate than caller 0", i)
		}
	}
	if n := fake.count("/new-order"); n != 1 {
		t.Errorf("%d orders posted for one cold host, want exactly 1", n)
	}
}

// TestHTTP01_FailedIssuanceCoolsDown pins the failure cooldown: after an
// order fails, the next handshakes take the remembered error instead of
// ordering again. Retrying per handshake would spend the five validation
// failures per hostname per hour that Let's Encrypt allows in seconds.
func TestHTTP01_FailedIssuanceCoolsDown(t *testing.T) {
	t.Parallel()
	m, fake := newPinnedManager(t)
	fake.mu.Lock()
	fake.rejectValidation = true
	fake.mu.Unlock()
	if err := m.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.stop()

	_, first := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "pin.example"})
	if first == nil {
		t.Fatal("issuance must fail when the CA rejects validation")
	}
	orders := fake.count("/new-order")
	if orders != 1 {
		t.Fatalf("%d orders for the first handshake, want 1", orders)
	}

	_, second := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "pin.example"})
	if second == nil {
		t.Fatal("second handshake must fail too")
	}
	if second != first { //nolint:errorlint // identity is the point: the cooled-down caller gets the remembered error value
		t.Errorf("second handshake got a fresh error %v, want the remembered %v", second, first)
	}
	if n := fake.count("/new-order"); n != orders {
		t.Errorf("orders grew to %d during the failure cooldown, want %d", n, orders)
	}
}

// TestHTTP01_InvalidAuthorizationFailsFast pins waiting on the
// authorization rather than the challenge. Only the authorization carries
// the identifier and its own terminal states, so an error raised from it
// names the host that failed; polling the challenge URL yields an
// acme.AuthorizationError with an empty Identifier, and leaves
// authorization-level states (expired, deactivated, revoked) invisible.
func TestHTTP01_InvalidAuthorizationFailsFast(t *testing.T) {
	t.Parallel()
	m, fake := newPinnedManager(t)
	fake.mu.Lock()
	fake.rejectValidation = true
	fake.mu.Unlock()
	if err := m.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.stop()

	done := make(chan error, 1)
	go func() {
		_, err := m.getOrIssue(context.Background(), "pin.example")
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("issuance did not return after the authorization went invalid; it polled to the deadline")
	}
	if err == nil {
		t.Fatal("issuance must fail on an invalid authorization")
	}
	var authzErr *acme.AuthorizationError
	if !errors.As(err, &authzErr) {
		t.Fatalf("error is %T (%v), want *acme.AuthorizationError", err, err)
	}
	if authzErr.Identifier != "pin.example" {
		t.Errorf("AuthorizationError.Identifier = %q, want %q", authzErr.Identifier, "pin.example")
	}
	if authzErr.URI != fake.url("/authz/1") {
		t.Errorf("AuthorizationError.URI = %q, want the authorization URL %q", authzErr.URI, fake.url("/authz/1"))
	}
}
