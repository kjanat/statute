package statute

import (
	"context"
	"fmt"
	"net"
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
	// conn is the UDP socket Start bound for Serve. quic-go leaves a
	// caller-provided PacketConn caller-owned — shutting the server down
	// does not close it — so Shutdown closes it once the drain completes.
	conn net.PacketConn
}

// Serve runs the HTTP/3 (QUIC) server on the given UDP socket and blocks
// until it stops. The caller binds the socket so a bind failure surfaces
// from Start, and so a failed Start can close the socket without closing
// the server — a closed http3.Server is not reusable, but one whose
// conn went away serves again on the next Serve call.
func (h *http3Listener) Serve(conn net.PacketConn) error {
	return h.srv.Serve(conn)
}

// Shutdown gracefully stops the HTTP/3 listener, then closes the UDP
// socket the server does not own.
func (h *http3Listener) Shutdown(ctx context.Context) error {
	err := h.srv.Shutdown(ctx)
	if h.conn != nil {
		_ = h.conn.Close()
	}
	return err
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
