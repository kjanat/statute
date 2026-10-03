package statute

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func TestHTTP3ProxyBodyError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		custom bool
	}{{"pool", false}, {"custom handler", true}} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthy" {
					_, _ = io.WriteString(w, "healthy")
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				// An unknown-length response must signal truncation through stream
				// cancellation; the client cannot infer it from Content-Length.
				_, _ = io.WriteString(w, "first")
				w.(http.Flusher).Flush()
				select {
				case <-release:
					panic(http.ErrAbortHandler)
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(origin.Close)
			udpAddr := startHTTP3ProxyTest(t, origin.URL, tc.custom)
			tr := &http3.Transport{TLSClientConfig: &tls.Config{
				ServerName: "h3.example", InsecureSkipVerify: true, //nolint:gosec // Hermetic self-signed server.
			}}
			t.Cleanup(func() { _ = tr.Close() })
			client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
			res, err := client.Get("https://" + udpAddr + "/stream")
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			prefix := make([]byte, 5)
			if _, err := io.ReadFull(res.Body, prefix); err != nil || string(prefix) != "first" || res.ProtoMajor != 3 {
				t.Fatalf("no HTTP/3 prefix: %q, %v", prefix, err)
			}
			close(release)
			if _, err := io.ReadAll(res.Body); !errors.Is(err, &http3.Error{Remote: true, ErrorCode: http3.ErrCodeInternalError}) {
				t.Fatalf("upstream truncation must reset the HTTP/3 stream: %v", err)
			}
			assertHTTP3Healthy(t, client, udpAddr)
		})
	}
}

func startHTTP3ProxyTest(t *testing.T, origin string, custom bool) string {
	t.Helper()
	certFile, keyFile := writeSelfSignedCert(t, "h3.example")
	tcpAddr, udpAddr := reserveAddr(t), reserveUDPAddr(t)
	cfg := Config{
		Listeners: Listeners{HTTPS(tcpAddr, StaticTLS(certFile, keyFile), HTTP3(udpAddr))},
		Shutdown:  Shutdown{GracePeriod: "2s"},
	}
	if custom {
		target, err := url.Parse(origin)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		transport := &http.Transport{}
		t.Cleanup(transport.CloseIdleConnections)
		proxy.Transport = transport
		cfg.Routes = Routes{Match("/*").Handle(proxy)}
	} else {
		cfg.Upstreams = Upstreams{"origin": Pool{Backends: []Backend{{Address: strings.TrimPrefix(origin, "http://")}}}}
		cfg.Routes = Routes{Match("/*").ProxyTo("origin")}
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return udpAddr
}

func assertHTTP3Healthy(t *testing.T, client *http.Client, addr string) {
	t.Helper()
	other, err := client.Get("https://" + addr + "/healthy")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Body.Close()
	body, err := io.ReadAll(other.Body)
	if err != nil || string(body) != "healthy" {
		t.Fatalf("later request failed: %q, %v", body, err)
	}
}
