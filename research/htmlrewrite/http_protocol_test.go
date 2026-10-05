//go:build statute_htmlrewrite

package htmlrewrite

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// protocolEndpoint uses real TCP/TLS or QUIC streams.
// It owns and joins the servers it creates; the caller still owns the engine.
func protocolEndpoint(t *testing.T, major int, handler http.Handler) (string, *http.Client) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	if major == 2 {
		client := server.Client()
		client.Timeout = 5 * time.Second
		t.Cleanup(client.CloseIdleConnections)
		return server.URL, client
	}
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quicServer := &http3.Server{
		Handler: handler, TLSConfig: &tls.Config{Certificates: server.TLS.Certificates, MinVersion: tls.VersionTLS13},
		// Match Statute's listener bridge: ReverseProxy must abort a failed
		// stream, and quic-go already recovers ErrAbortHandler with a reset.
		ConnContext: func(ctx context.Context, _ *quic.Conn) context.Context {
			return context.WithValue(ctx, http.ServerContextKey, server.Config)
		},
	}
	done := make(chan error, 1)
	go func() { done <- quicServer.Serve(conn) }()
	t.Cleanup(func() {
		_ = quicServer.Close()
		_ = conn.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("HTTP/3 server shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("HTTP/3 server did not join")
		}
	})
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http3.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}
	t.Cleanup(func() { _ = transport.Close() })
	return "https://" + conn.LocalAddr().String(), &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestHTTPMultiplexedStreamInterruption(t *testing.T) {
	for _, major := range []int{2, 3} {
		for _, failure := range []failurePolicy{failClosed, failOpen} {
			for _, outcome := range []string{"disconnect", "output limit"} {
				t.Run(fmt.Sprintf("HTTP%d/policy%d/%s", major, failure, outcome), func(t *testing.T) {
					originDone := make(chan struct{})
					release := make(chan struct{})
					origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer close(originDone)
						w.Header().Set("Content-Type", "text/html")
						_, _ = io.WriteString(w, `<a class="rewrite">prefix</a>`)
						w.(http.Flusher).Flush()
						select {
						case <-release:
							_, _ = io.WriteString(w, strings.Repeat("tail", 4096))
						case <-r.Context().Done():
						}
					}))
					t.Cleanup(origin.Close)
					e := httpTestEngine(t, 1)
					p := testHTTPPolicy(failure)
					p.outputLimit = 1024
					base := &http.Transport{DisableCompression: true}
					t.Cleanup(base.CloseIdleConnections)
					rt := httpTestTransport(t, e, base, p)
					proxy := httpTestProxy(t, origin.URL, rt)
					proxyDone := make(chan struct{})
					endpoint, client := protocolEndpoint(t, major, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer close(proxyDone)
						proxy.ServeHTTP(w, r)
					}))
					res, err := client.Get(endpoint)
					if err != nil {
						t.Fatal(err)
					}
					defer res.Body.Close()
					if res.ProtoMajor != major {
						t.Fatalf("negotiated %s instead of HTTP/%d", res.Proto, major)
					}
					prefix := make([]byte, len(`<a class="rewrite" href="https://example.invalid/rewritten">prefix<em>inserted</em>`))
					if _, err := io.ReadFull(res.Body, prefix); err != nil || !bytes.Contains(prefix, []byte("inserted")) {
						t.Fatalf("no rewritten prefix before EOF: %q, %v", prefix, err)
					}
					if outcome == "disconnect" {
						_ = res.Body.Close()
					} else {
						close(release)
						_, err := io.ReadAll(res.Body)
						if err == nil || errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("expected immediate stream failure: %v", err)
						}
						if major == 3 && !errors.Is(err, &http3.Error{Remote: true, ErrorCode: http3.ErrCodeInternalError}) {
							t.Fatalf("expected HTTP/3 stream reset: %v", err)
						}
						if rt.failed.Load() != 1 || rt.bypassed.Load() != 0 {
							t.Fatal("body-time failure became a bypass")
						}
					}
					for _, done := range []chan struct{}{originDone, proxyDone} {
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Fatal("interruption left a handler running")
						}
					}
					e.mu.Lock()
					active := len(e.active)
					e.mu.Unlock()
					if active != 0 {
						t.Fatal("interruption retained a Wasm instance")
					}
				})
			}
		}
	}
}
