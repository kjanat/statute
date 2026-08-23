package statute

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

	mu         sync.Mutex
	authzValid bool
	accepted   []string // challenge types the client accepted
	certPEM    []byte   // issued chain, filled by signCSR
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
	f := &fakeACME{t: t, caKey: caKey, caCert: caCert, challenge: challenge}
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
		f.mu.Lock()
		status := "pending"
		if f.authzValid {
			status = "valid"
		}
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, map[string]any{
			"status":     status,
			"identifier": map[string]string{"type": "dns", "value": "pin.example"},
			"challenges": []map[string]string{
				{"type": "tls-alpn-01", "url": f.url("/chal/alpn"), "token": fakeACMEToken, "status": "pending"},
				{"type": "http-01", "url": f.url("/chal/http"), "token": fakeACMEToken, "status": "pending"},
			},
		})
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

// handleChallenge answers the challenge endpoints. A POST-as-GET (empty
// JWS payload) is WaitAuthorization polling the challenge status; a
// non-empty payload ({}) is Accept, on which the fake CA validates.
func (f *fakeACME) handleChallenge(w http.ResponseWriter, r *http.Request) {
	typ := "http-01"
	if r.URL.Path == "/chal/alpn" {
		typ = "tls-alpn-01"
	}
	if f.jwsPayload(r) == nil {
		f.mu.Lock()
		status := "pending"
		if f.authzValid {
			status = "valid"
		}
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, map[string]string{
			"type": typ, "url": f.url(r.URL.Path), "token": fakeACMEToken, "status": status,
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
	f.signCSR(csr)
	f.writeJSON(w, http.StatusOK, map[string]any{
		"status":      "valid",
		"finalize":    f.url("/finalize/1"),
		"certificate": f.url("/cert/1"),
	})
}

// validateHTTP01 plays the CA's validation role: fetch the key
// authorization from the challenge handler under test.
func (f *fakeACME) validateHTTP01() {
	req := httptest.NewRequest("GET", "http://pin.example/.well-known/acme-challenge/"+fakeACMEToken, nil)
	rec := httptest.NewRecorder()
	f.challenge.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(body, fakeACMEToken+".") {
		f.t.Errorf("fake acme: http-01 validation failed: code=%d body=%q", rec.Code, body)
		return
	}
	f.mu.Lock()
	f.authzValid = true
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
// HTTP listener's handler chain, and come out of start() with a routable
// certificate that a fresh manager then reloads from disk without touching
// ACME again.
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
