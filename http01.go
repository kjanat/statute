package statute

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"golang.org/x/crypto/acme"

	"statute.kjanat.dev/resolved"
)

// newHTTP01Manager builds an acmeManager that satisfies HTTP-01 challenges
// by serving each key authorization on the plain HTTP listeners (wired via
// wrapHTTPChallenges). It exists because autocert cannot be pinned: its
// challenge preference is hard-coded to try TLS-ALPN-01 first, so an
// AutoTLS source declared HTTP01() would burn a deliberately failed
// TLS-ALPN validation on every fresh authorization. This manager only ever
// attempts HTTP-01. Its storage lives under <storage>/http01.
func newHTTP01Manager(cfg *resolved.AutoTLS) (*acmeManager, error) {
	return newACMEManager(cfg, "http01", newHTTP01Solver())
}

// http01Solver satisfies ACME HTTP-01 challenges by publishing each key
// authorization in an in-memory token table that the plain HTTP listeners
// consult under /.well-known/acme-challenge/.
type http01Solver struct {
	mu     sync.RWMutex
	tokens map[string]string // challenge URL path -> key authorization body
}

func newHTTP01Solver() *http01Solver {
	return &http01Solver{tokens: make(map[string]string)}
}

func (*http01Solver) challengeType() string { return "http-01" }

func (s *http01Solver) satisfy(ctx context.Context, client *acme.Client, _ string, ch *acme.Challenge) error {
	body, err := client.HTTP01ChallengeResponse(ch.Token)
	if err != nil {
		return err
	}
	path := client.HTTP01ChallengePath(ch.Token)
	s.mu.Lock()
	s.tokens[path] = body
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.tokens, path)
		s.mu.Unlock()
	}()

	if _, err := client.Accept(ctx, ch); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, ch.URI); err != nil {
		return fmt.Errorf("wait authorization: %w", err)
	}
	return nil
}

// wrap serves pending challenge responses ahead of next, passing every
// other request through untouched — the same shape as autocert's
// HTTPHandler, minus its HTTPS redirect.
func (s *http01Solver) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		body, ok := s.tokens[r.URL.Path]
		s.mu.RUnlock()
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	})
}
