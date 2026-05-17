package statute

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Pre-computed bcrypt hashes (cost=10) for the test credentials so the
// suite does not pay the ~80ms-per-hash generation cost on every run.
const (
	// password: "hunter2"
	hunter2Hash = "$2a$10$HwrzUQtDrRX0/09su3BahezCIqD.f4HjCkYD5b9w8gl4eUkPJzCyu"
	// password: "letmein"
	letmeinHash = "$2a$10$6aCr4XBcW8QMeJtTZ58Og.WbJ8FXadUl5JpatvrfEw5b2oV6MfH5O"
)

func TestBasicAuth_NoCredentials_Returns401(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("upstream invoked without credentials")
	})
	h := basicAuthHandler(mustResolveMW(t, BasicAuth("realm", map[string]string{"alice": hunter2Hash})), inner)

	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Errorf("WWW-Authenticate header missing")
	}
}

func TestBasicAuth_WrongPassword_Returns401(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("upstream invoked")
	})
	h := basicAuthHandler(mustResolveMW(t, BasicAuth("realm", map[string]string{"alice": hunter2Hash})), inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.SetBasicAuth("alice", "wrong")
	rec := runRequest(t, h, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: %d", rec.Code)
	}
}

func TestBasicAuth_CorrectCredentials_PassThrough(t *testing.T) {
	t.Parallel()
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := basicAuthHandler(mustResolveMW(t, BasicAuth("realm", map[string]string{
		"alice": hunter2Hash,
		"bob":   letmeinHash,
	})), inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.SetBasicAuth("bob", "letmein")
	rec := runRequest(t, h, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	if !called {
		t.Errorf("upstream not invoked")
	}
}

func TestBasicAuth_UnknownUser_Returns401(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	h := basicAuthHandler(mustResolveMW(t, BasicAuth("realm", map[string]string{"alice": hunter2Hash})), inner)
	req := httptest.NewRequest("GET", "/", nil)
	req.SetBasicAuth("eve", "anything")
	rec := runRequest(t, h, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: %d", rec.Code)
	}
}

func TestBasicAuth_PlaintextRejectedAtResolve(t *testing.T) {
	t.Parallel()
	_, err := resolveMiddleware(BasicAuth("realm", map[string]string{"alice": "hunter2-plaintext"}))
	if err == nil {
		t.Fatal("want error for plaintext password")
	}
}

func TestBasicAuth_EmptyUsersRejectedAtResolve(t *testing.T) {
	t.Parallel()
	_, err := resolveMiddleware(BasicAuth("realm", map[string]string{}))
	if err == nil {
		t.Fatal("want error for empty users")
	}
}

func TestIsBCryptHash(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    string
		want bool
	}{
		{hunter2Hash, true},
		{letmeinHash, true},
		{"$2a$10$invalid-content", false}, // wrong base64
		{"plaintext", false},
		{"", false},
		{"$1$abc$xyz", false}, // md5 unix crypt, not bcrypt
	}
	for _, c := range cases {
		got := isBCryptHash(c.s)
		if got != c.want {
			t.Errorf("isBCryptHash(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}
