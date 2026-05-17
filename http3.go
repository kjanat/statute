package statute

import (
	"context"
	"crypto/tls"
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

func (h *http3Listener) Serve() error {
	return h.srv.ListenAndServe()
}

func (h *http3Listener) Shutdown(ctx context.Context) error {
	return h.srv.Shutdown(ctx)
}

func (s *server) buildHTTP3Server(l *resolved.Listener, content http.Handler) (*http3Listener, error) {
	addr := strings.TrimSuffix(l.HTTP3Addr, "/udp")
	if addr == "" {
		return nil, fmt.Errorf("http3 listener address is empty")
	}

	var tlsCfg *tls.Config
	switch {
	case l.AutoTLS != nil && l.AutoTLS.DNS01 != nil:
		dm := s.dns01Managers[l.Addr]
		if dm == nil {
			return nil, fmt.Errorf("auto_tls: dns01 manager not initialised")
		}
		tlsCfg = dns01TLSConfig(dm, true)
	case l.AutoTLS != nil:
		if s.autocertMgr == nil {
			return nil, fmt.Errorf("auto_tls: manager not initialised")
		}
		tlsCfg = autocertTLSConfig(s.autocertMgr, true, l.BehindCloudflare)
	case l.StaticTLS != nil:
		cert, err := tls.LoadX509KeyPair(l.StaticTLS.CertFile, l.StaticTLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load static TLS material: %w", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}}
	default:
		return nil, fmt.Errorf("http3 listener requires AutoTLS or StaticTLS on the parent HTTPS listener")
	}

	// HTTP/3 mandates ALPN "h3".
	tlsCfg = tlsCfg.Clone()
	tlsCfg.NextProtos = []string{"h3"}

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
