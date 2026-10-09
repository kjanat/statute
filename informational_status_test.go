package statute

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"statute.kjanat.dev/resolved"
)

//nolint:gocyclo // one protocol/wrapper matrix verifies the final wire status and observation together.
func TestMultiplexedInformational101FinalStatus(t *testing.T) {
	t.Parallel()
	for _, proto := range []string{"h2", "h3"} {
		for _, tc := range []struct {
			name string
			mws  []Middleware
		}{
			{"plain", nil},
			{"timeout", []Middleware{Timeout("5s")}},
			{"retry", []Middleware{Retry(2)}},
			{"etag", []Middleware{ETag()}},
			{"cache", []Middleware{Cache("1m")}},
			{"compress", []Middleware{Compress(Gzip)}},
			{"nested", []Middleware{Timeout("5s"), Retry(2), Cache("1m"), ETag(), Compress(Gzip)}},
		} {
			t.Run(proto+"/"+tc.name, func(t *testing.T) {
				st := newStats()
				h := metricsMiddleware(st, chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusSwitchingProtocols)
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, "denied")
				}), tc.mws...))
				var client *http.Client
				var target string
				if proto == "h2" {
					front := httptest.NewUnstartedServer(h)
					front.EnableHTTP2 = true
					front.StartTLS()
					t.Cleanup(front.Close)
					client, target = front.Client(), front.URL
				} else {
					certFile, keyFile := writeSelfSignedCert(t, "h3.example")
					udpAddr := reserveUDPAddr(t)
					srv, err := newServer(mustResolve(t, Config{
						Listeners: Listeners{HTTPS(reserveAddr(t), StaticTLS(certFile, keyFile), HTTP3(udpAddr))},
						Routes:    Routes{Match("/*").Handle(h)},
					}))
					if err != nil {
						t.Fatal(err)
					}
					if err := srv.Start(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = srv.Shutdown() })
					tr := &http3.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only local certificate
					t.Cleanup(func() { _ = tr.Close() })
					client, target = &http.Client{Transport: tr, Timeout: 5 * time.Second}, "https://"+udpAddr
				}
				resp, err := client.Get(target)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusUnauthorized || string(body) != "denied" {
					t.Fatalf("response=%d %q error=%v", resp.StatusCode, body, err)
				}
				wantMajor := 2
				if proto == "h3" {
					wantMajor = 3
				}
				if resp.ProtoMajor != wantMajor {
					t.Fatalf("negotiated %s", resp.Proto)
				}
				var metrics bytes.Buffer
				st.WritePrometheus(&metrics)
				if !strings.Contains(metrics.String(), `statute_requests_by_status_total{status="401"} 1`) {
					t.Fatalf("wrong metrics: %s", metrics.String())
				}
			})
		}
	}
}

func TestHTTP2Ignored101DoesNotCommitCachePolicy(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusSwitchingProtocols)
		w.Header().Del("Cache-Control")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "public")
	}), Cache("1m"), ETag())
	front := httptest.NewUnstartedServer(h)
	front.EnableHTTP2 = true
	front.StartTLS()
	t.Cleanup(front.Close)
	for range 2 {
		resp, err := front.Client().Get(front.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 || string(body) != "public" {
			t.Fatalf("response=%d %q protocol=%s error=%v", resp.StatusCode, body, resp.Proto, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("ignored 101 poisoned cache admission: calls=%d", calls.Load())
	}
}

func TestNativeProxyRejectsInformational101(t *testing.T) {
	t.Parallel()
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("upstream protocol: %s", r.Proto)
		}
		w.WriteHeader(http.StatusSwitchingProtocols)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "denied")
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	t.Cleanup(origin.Close)
	for _, h2 := range []bool{false, true} {
		name := "h1"
		if h2 {
			name = "h2"
		}
		t.Run(name, func(t *testing.T) {
			pool, err := newPoolHandler(&resolved.Pool{
				Backends:  []resolved.Backend{{Address: origin.URL, Weight: 1}},
				Transport: resolved.Transport{InsecureSkipVerify: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.transport.CloseIdleConnections)
			st := newStats()
			front := httptest.NewUnstartedServer(metricsMiddleware(st, pool))
			if h2 {
				front.EnableHTTP2 = true
				front.StartTLS()
			} else {
				front.Start()
			}
			t.Cleanup(front.Close)
			resp, err := front.Client().Get(front.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			var metrics bytes.Buffer
			st.WritePrometheus(&metrics)
			if !strings.Contains(metrics.String(), `statute_requests_by_status_total{status="502"} 1`) {
				t.Fatalf("wrong metrics: %s", metrics.String())
			}
		})
	}
}
