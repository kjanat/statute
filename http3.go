package statute

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/quic-go/quic-go/http3"

	"statute.kjanat.dev/resolved"
)

// http3Listener wraps a quic-go http3.Server with a Serve/Shutdown interface
// matching the rest of the runtime.
type http3Listener struct {
	srv  *http3.Server
	addr string
}

// Serve starts the HTTP/3 (QUIC) listener and blocks until it stops.
func (h *http3Listener) Serve() error {
	return h.srv.ListenAndServe()
}

// Shutdown gracefully stops the HTTP/3 listener.
func (h *http3Listener) Shutdown(ctx context.Context) error {
	return h.srv.Shutdown(ctx)
}

func (s *server) buildHTTP3Server(l *resolved.Listener, content http.Handler) (*http3Listener, error) {
	addr := strings.TrimSuffix(l.HTTP3Addr, "/udp")
	if addr == "" {
		return nil, fmt.Errorf("http3 listener address is empty")
	}

	// The parent HTTPS listener's cert router (stored by applyListenerTLS,
	// which initListeners runs first) dispatches certificates for QUIC
	// handshakes exactly as it does for TCP.
	cr := s.certRouters[l.Addr]
	if cr == nil {
		return nil, fmt.Errorf("http3 listener requires AutoTLS or StaticTLS on the parent HTTPS listener")
	}
	tlsCfg := certRouterTLSConfig(cr, l)
	// HTTP/3 mandates ALPN "h3".
	tlsCfg.NextProtos = []string{alpnHTTP3}

	srv := &http3.Server{
		Addr:      addr,
		Handler:   content,
		TLSConfig: tlsCfg,
	}
	return &http3Listener{srv: srv, addr: addr}, nil
}

// altSvcHandler advertises the HTTP/3 endpoint on every HTTPS response so
// compatible clients upgrade subsequent requests to HTTP/3. ma is one day,
// matching common practice; lower values are appropriate while a deployment
// is still validating its HTTP/3 path.
func altSvcHandler(http3Addr string, next http.Handler) http.Handler {
	addr := strings.TrimSuffix(http3Addr, "/udp")
	port := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		port = addr[i+1:]
	}
	value := fmt.Sprintf(`h3=":%s"; ma=86400`, port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", value)
		next.ServeHTTP(w, r)
	})
}
