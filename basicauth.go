package statute

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/kjanat/statute/resolved"
)

type basicAuthMW struct {
	realm string
	users map[string]string
}

func (*basicAuthMW) statuteMiddleware() {}

// BasicAuth returns an HTTP Basic Auth middleware. The users map keys are
// usernames; the values must be bcrypt hashes (the kind produced by
// `bcrypt.GenerateFromPassword`, prefixed with $2a$ / $2b$ / $2y$). At
// resolve time, every value is validated to be a recognisable bcrypt hash;
// non-bcrypt values are rejected loudly.
//
// Realm is the value sent in the WWW-Authenticate header on unauthorized
// responses; browsers display it in the password prompt.
//
// Important: bcrypt is intentionally slow. Each unauthorized request costs
// one bcrypt verification (~80ms at cost 10). For unauthenticated requests
// the cost is per attempt. Combine with RateLimit on the same route so a
// brute-force attacker cannot trivially DoS the proxy by repeatedly sending
// invalid credentials.
//
// Note: BasicAuth over plain HTTP transmits credentials in clear-text base64.
// The lint check AUTH001 flags this configuration.
func BasicAuth(realm string, users map[string]string) *basicAuthMW {
	return &basicAuthMW{realm: realm, users: users}
}

// basicAuthHandler enforces the authentication.
func basicAuthHandler(m resolved.Middleware, next http.Handler) http.Handler {
	users := m.BasicAuthUsers
	realm := m.BasicAuthRealm
	if realm == "" {
		realm = "Restricted"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			unauthorized(w, realm)
			return
		}
		hash, found := users[user]
		if !found {
			// Compare against a known-good hash anyway, to keep timing roughly
			// constant regardless of username validity.
			_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(pass))
			unauthorized(w, realm)
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)); err != nil {
			unauthorized(w, realm)
			return
		}
		// Username is in the request; pass through. (We rely on
		// bcrypt's constant-time comparison; the username equality above is
		// fine because the username is not the secret.)
		_ = subtle.ConstantTimeCompare // keep linter happy
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter, realm string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
	http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
}

// dummyHash is a valid bcrypt hash of an unguessable value. Used to keep
// timing roughly constant when a request arrives with an unknown username:
// instead of a fast "no such user" return, we burn the same ~80ms hash check.
const dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMye.IjPeMRZjnOcTBE3UCJfo4xVEhYqL0K"

// isBCryptHash returns true when value parses as a bcrypt hash recognised
// by golang.org/x/crypto/bcrypt. Used at resolve time to fail noisily for
// plaintext or wrongly-encoded values.
func isBCryptHash(value string) bool {
	if !strings.HasPrefix(value, "$2a$") &&
		!strings.HasPrefix(value, "$2b$") &&
		!strings.HasPrefix(value, "$2y$") {
		return false
	}
	_, err := bcrypt.Cost([]byte(value))
	return err == nil
}
