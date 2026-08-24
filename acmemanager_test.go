package statute

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"statute.kjanat.dev/resolved"
)

// stubSolver stands in for a real challenge solver. It records what
// solveAuthorizations hands it and never accepts the challenge, so the
// order's authorization stays pending — the state that costs the account
// one of its 300 pending authorizations for a week.
type stubSolver struct {
	typ string
	err error
	// hold blocks satisfy until the issuance context is cancelled, then
	// keeps working for linger. It models a solver still talking to a DNS
	// API or holding a token when the server shuts down.
	hold   bool
	linger time.Duration

	entered  sync.Once
	enteredC chan struct{}
	returned atomic.Bool

	mu           sync.Mutex
	authzURL     string
	challengeURI string
}

func (s *stubSolver) challengeType() string { return s.typ }

func (s *stubSolver) satisfy(ctx context.Context, _ *acme.Client, _, authzURL string, ch *acme.Challenge) error {
	s.mu.Lock()
	s.authzURL, s.challengeURI = authzURL, ch.URI
	s.mu.Unlock()
	s.entered.Do(func() { close(s.enteredC) })
	if s.hold {
		<-ctx.Done()
		time.Sleep(s.linger)
	}
	s.returned.Store(true)
	return s.err
}

func (s *stubSolver) seen() (authzURL, challengeURI string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authzURL, s.challengeURI
}

// startStubManager builds a started manager over the stub solver, wired to a
// fake CA that never validates anything.
func startStubManager(t *testing.T, solver *stubSolver) (*acmeManager, *acmeRun, *fakeACME) {
	t.Helper()
	solver.enteredC = make(chan struct{})
	m, err := newACMEManager(&resolved.AutoTLS{
		Domains: []string{"pin.example"},
		Email:   "ops@pin.example",
		Storage: t.TempDir(),
	}, "stub", solver)
	if err != nil {
		t.Fatalf("newACMEManager: %v", err)
	}
	fake := newFakeACME(t, nil)
	m.directoryURL = fake.url("/dir")
	run, err := m.start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(run.stop)
	return m, run, fake
}

// TestACMEManager_ServesCertificateInsideRenewalWindow pins the split
// between serving and renewing. A certificate with 20 days left is inside
// the renewal window but still perfectly valid TLS material, so the
// handshake must be served from it. Gating the cache on renewal freshness
// instead put a full ACME order in the handshake path for a month before
// expiry — and, as here, failed the handshake outright whenever the CA was
// unreachable, despite a working certificate being right there.
func TestACMEManager_ServesCertificateInsideRenewalWindow(t *testing.T) {
	t.Parallel()
	cert := testCertificate(t, time.Now().Add(20*24*time.Hour))
	if !needsRenewal(cert) {
		t.Fatal("fixture must be inside the renewal window")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := &acmeManager{
		name:    "dns01",
		domains: []string{"pin.example"},
		storage: t.TempDir(),
		cache:   map[string]*tls.Certificate{"pin.example": cert},
		// Nothing listens on this address: any attempt to issue fails
		// instead of quietly succeeding.
		acmeClient: &acme.Client{Key: key, DirectoryURL: "http://" + reserveAddr(t) + "/dir"},
	}

	got, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "pin.example"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got != cert {
		t.Error("GetCertificate did not serve the cached certificate")
	}
}

// TestACMEManager_FailedOrderDeactivatesPendingAuthz covers the cleanup an
// abandoned order owes the account: an authorization left pending blocks a
// slot of the 300-pending limit for seven days, and a solver that fails
// before accepting the challenge leaves exactly that. It also pins which
// URL the solver is handed — the authorization's, not the challenge's.
func TestACMEManager_FailedOrderDeactivatesPendingAuthz(t *testing.T) {
	t.Parallel()
	solver := &stubSolver{typ: "http-01", err: os.ErrPermission}
	m, _, fake := startStubManager(t, solver)

	if _, err := m.getOrIssue(context.Background(), "pin.example"); err == nil {
		t.Fatal("issuance must fail when the solver fails")
	}

	authzURL, challengeURI := solver.seen()
	if authzURL != fake.url("/authz/1") {
		t.Errorf("solver got authzURL %q, want the authorization URL %q", authzURL, fake.url("/authz/1"))
	}
	if challengeURI == authzURL {
		t.Error("solver got the same URL for the challenge and the authorization")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := fake.deactivations(); len(got) > 0 {
			if got[0] != fake.url("/authz/1") {
				t.Errorf("deactivated %q, want %q", got[0], fake.url("/authz/1"))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the order's pending authorization was never deactivated")
}

// TestACMEManager_StopWaitsForWarm pins the shutdown contract for the
// background warm-up: HTTP-01 managers warm in a goroutine after the
// listeners open, and stop must not return while that goroutine is still
// inside the ACME flow. Anything it logs afterwards lands in a process —
// or a test — that believes it has finished.
func TestACMEManager_StopWaitsForWarm(t *testing.T) {
	t.Parallel()
	solver := &stubSolver{typ: "http-01", err: os.ErrPermission, hold: true, linger: 100 * time.Millisecond}
	_, run, _ := startStubManager(t, solver)

	run.warmAsync()
	select {
	case <-solver.enteredC:
	case <-time.After(10 * time.Second):
		t.Fatal("warm-up never reached the solver")
	}
	run.stop()
	if !solver.returned.Load() {
		t.Error("stop returned while the warm-up was still running")
	}
}

// TestACMEManager_StartTwice pins the guard on the lifecycle fields. A
// second start would install a new run context and done channel over the
// running loop's, orphaning it and leaving stop to close an already closed
// channel.
func TestACMEManager_StartTwice(t *testing.T) {
	t.Parallel()
	m, first, _ := startStubManager(t, &stubSolver{typ: "http-01"})
	unexpected, err := m.start()
	if err == nil {
		unexpected.stop()
		t.Fatal("starting an already started manager must fail")
	}
	// Stopping releases the lifecycle, so a restart is allowed again.
	first.stop()
	second, err := m.start()
	if err != nil {
		t.Fatalf("restart after stop: %v", err)
	}
	second.stop()
}

// TestACMERun_StoppedGenerationCannotAffectRestart proves that cancellation
// and warm-up belong to the returned run, not mutable manager fields. A stale
// handle cannot cancel or perform work through the later generation.
func TestACMERun_StoppedGenerationCannotAffectRestart(t *testing.T) {
	t.Parallel()
	m, first, fake := startStubManager(t, &stubSolver{typ: "http-01"})
	first.stop()
	if m.runContext().Err() == nil {
		t.Fatal("stopped manager admitted unowned ACME work between runs")
	}
	second, err := m.start()
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer second.stop()

	first.stop()
	first.warm()
	if second.ctx.Err() != nil {
		t.Fatal("stale stop cancelled the later ACME run")
	}
	if got := fake.count("/new-order"); got != 0 {
		t.Fatalf("stale warm-up created %d orders through the later run", got)
	}
}

// TestACMEManager_PersistCertWritesAPair pins atomic persistence: the
// chain and key land as a matching pair loadCert can read back, and no
// temp file survives. Two plain writes could leave a chain beside a
// stranger's key, which tls.X509KeyPair rejects and loadCert then reports
// as no certificate at all.
func TestACMEManager_PersistCertWritesAPair(t *testing.T) {
	t.Parallel()
	m := &acmeManager{name: "dns01", storage: t.TempDir()}
	cert := testCertificate(t, time.Now().Add(90*24*time.Hour))
	if err := m.persistCert("pin.example", cert.Certificate, testKey(t, cert)); err != nil {
		t.Fatalf("persistCert: %v", err)
	}

	loaded := m.loadCert("pin.example")
	if loaded == nil {
		t.Fatal("loadCert found nothing after persistCert")
	}
	if !slices.EqualFunc(loaded.Certificate, cert.Certificate, slices.Equal) {
		t.Error("reloaded chain differs from the persisted one")
	}

	entries, err := os.ReadDir(m.storage)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"pin.example.crt", "pin.example.key"}) {
		t.Errorf("storage holds %v, want just the certificate pair", names)
	}
}

// TestACMEManager_RenewalRefreshesCache pins what a successful renewal
// owes the running server: the new certificate must reach the in-memory
// cache handshakes are served from. Routing renewal through the same
// issuance path as a handshake is what guarantees it — re-reading the
// certificate from disk instead keeps serving the old one for as long as
// persistence is failing, and re-orders every hour meanwhile.
func TestACMEManager_RenewalRefreshesCache(t *testing.T) {
	t.Parallel()
	m, _ := newPinnedManager(t)
	run, err := m.start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer run.stop()

	expiring := testCertificate(t, time.Now().Add(7*24*time.Hour))
	m.cache["pin.example"] = expiring

	run.renewExpiring()

	m.mu.RLock()
	got := m.cache["pin.example"]
	m.mu.RUnlock()
	if got == expiring {
		t.Fatal("renewal left the expiring certificate in the cache")
	}
	if needsRenewal(got) {
		t.Error("the certificate left in the cache is itself due for renewal")
	}
}

// TestACMEManager_PersistCertSurvivesAPartialWrite injects a failure into
// the middle of persistence: the stored key file is made read-only, so a
// writer that truncates each destination in turn replaces the chain and
// then fails on the key, leaving a chain that no key on disk matches —
// which tls.X509KeyPair rejects and loadCert can only report as no
// certificate at all. A crash between the two writes leaves exactly the
// same wreckage, permanently. Writing both files aside and renaming them
// into place keeps a loadable pair on disk either way.
func TestACMEManager_PersistCertSurvivesAPartialWrite(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this test injects the failure with")
	}
	m := &acmeManager{name: "dns01", storage: t.TempDir()}
	first := testCertificate(t, time.Now().Add(90*24*time.Hour))
	if err := m.persistCert("pin.example", first.Certificate, testKey(t, first)); err != nil {
		t.Fatalf("persistCert: %v", err)
	}
	if err := os.Chmod(filepath.Join(m.storage, "pin.example.key"), 0o400); err != nil {
		t.Fatal(err)
	}

	second := testCertificate(t, time.Now().Add(90*24*time.Hour))
	// The error is not the point — the state it leaves behind is.
	_ = m.persistCert("pin.example", second.Certificate, testKey(t, second))

	if m.loadCert("pin.example") == nil {
		t.Fatal("after a failed persist the stored chain and key no longer match")
	}
}

func testKey(t *testing.T, cert *tls.Certificate) *ecdsa.PrivateKey {
	t.Helper()
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("fixture key is %T", cert.PrivateKey)
	}
	return key
}
