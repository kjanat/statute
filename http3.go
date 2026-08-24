package statute

import (
	"context"
	"errors"
	"fmt"
	"log"
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

// serveLoop runs the HTTP/3 (QUIC) server on the socket Start bound into
// h.conn and blocks until it stops. Start binds the socket so a bind
// failure fails Start instead of vanishing here, and so a failed Start
// can close the socket without closing the server — a closed http3.Server
// is not reusable, but one whose conn went away serves again on the next
// call. If the loop dies for any reason other than shutdown, the socket
// is closed too: a dead server must not keep the caller-owned UDP port
// bound — and Alt-Svc keeps advertising it — for the life of the process.
func (h *http3Listener) serveLoop() {
	err := h.srv.Serve(h.conn)
	if isServeShutdown(err) {
		return
	}
	log.Printf("statute: http3 %s: serve loop exited: %v", h.addr, err)
	// Shutdown surfaces its close failure in the returned error; this
	// exceptional path has no caller, so the log is the only witness
	// that the port may still be bound.
	if cerr := h.conn.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
		log.Printf("statute: http3 %s: closing socket after dead serve loop: %v", h.addr, cerr)
	}
}

// Shutdown gracefully stops the HTTP/3 listener, then closes the UDP
// socket the server does not own. A close failure joins the shutdown
// error — "this port may still be bound" is not a detail to swallow.
func (h *http3Listener) Shutdown(ctx context.Context) error {
	err := h.srv.Shutdown(ctx)
	if h.conn != nil {
		if cerr := h.conn.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = errors.Join(err, cerr)
		}
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
