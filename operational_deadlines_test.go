package statute

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

func TestOperationalListenerDefaults(t *testing.T) {
	t.Parallel()
	for _, defaults := range []Defaults{
		{},
		{ReadHeaderTimeout: "1s", ReadTimeout: "2s", WriteTimeout: "3s", IdleTimeout: "4s", MaxHeaderBytes: 2048},
		{ReadHeaderTimeout: "0s", ReadTimeout: "0s", WriteTimeout: "0s", IdleTimeout: "0s"},
	} {
		cfg := operationalDeadlineConfig(defaults)
		rc := mustResolve(t, cfg)
		srv, err := newServer(rc)
		if err != nil {
			t.Fatal(err)
		}
		for _, hs := range []*http.Server{srv.buildHealthServer(rc.Observability.Health), srv.metricsServer} {
			got := resolved.Defaults{
				ReadHeaderTimeout: hs.ReadHeaderTimeout, ReadTimeout: hs.ReadTimeout,
				WriteTimeout: hs.WriteTimeout, IdleTimeout: hs.IdleTimeout, MaxHeaderBytes: hs.MaxHeaderBytes,
			}
			if !reflect.DeepEqual(got, rc.Defaults) {
				t.Fatalf("%s: defaults=%+v, want %+v", hs.Addr, got, rc.Defaults)
			}
		}
	}
}

func operationalDeadlineConfig(defaults Defaults) Config {
	cfg := redirectHealthConfig("127.0.0.1:0")
	cfg.Defaults = defaults
	cfg.Observability.Metrics = Prometheus("127.0.0.1:0", "/metrics")
	return cfg
}

func TestOperationalListenerConnectionDeadlines(t *testing.T) {
	for _, incompleteBody := range []bool{false, true} {
		cfg := operationalDeadlineConfig(Defaults{ReadTimeout: "200ms", IdleTimeout: "100ms"})
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
		for path, addr := range map[string]string{
			"/healthz": srv.run.listeners.health.listener.Addr().String(),
			"/metrics": srv.run.listeners.metrics.listener.Addr().String(),
		} {
			assertOperationalDeadline(t, addr, path, incompleteBody)
		}
		mustServeHealth(t, srv.run.listeners.health.listener.Addr().String())
		mustServeMetrics(t, srv.run.listeners.metrics.listener.Addr().String())
	}
}

func assertOperationalDeadline(t *testing.T, addr, path string, incompleteBody bool) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	request := "GET " + path + " HTTP/1.1\r\nHost: localhost\r\n"
	if incompleteBody {
		request += "Content-Length: 2\r\n\r\nx"
	} else {
		request += "\r\n"
	}
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if !incompleteBody {
		assertOperationalKeepAlive(t, reader)
	}
	_, err = io.Copy(io.Discard, reader)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("%s: server left connection open until client deadline (incomplete body=%t)", path, incompleteBody)
	}
}

func assertOperationalKeepAlive(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	res, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || res.Close {
		t.Fatalf("expected successful keep-alive response: %v, status=%d close=%t", err, res.StatusCode, res.Close)
	}
}
