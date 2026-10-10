package statute

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// TestClientHostRejectsUnusableHTTP2AuthorityBeforeHTTP1Backend proves invalid
// HTTP/2 authorities cannot select the HTTP/1 backend's default virtual host.
func TestClientHostRejectsUnusableHTTP2AuthorityBeforeHTTP1Backend(t *testing.T) {
	var backendRequests atomic.Int64
	backend := httptest.NewServer(clientHostVirtualHosts(&backendRequests))
	t.Cleanup(backend.Close)

	cfg := hostPoolConfig(backend.URL, ClientHost)
	pool := cfg.Upstreams["api"]
	pool.PassiveHealthCheck = PassiveHealthCheck{FailureWindow: "1m", MaxFailures: 1}
	cfg.Upstreams["api"] = pool
	cfg.Routes = Routes{
		Match("/retry/*").ProxyTo("api").With(Retry(3, OnStatus(http.StatusBadRequest))),
		Match("/*").ProxyTo("api"),
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	ph := srv.pools["api"]
	running := ph.start()
	t.Cleanup(running.shutdown)

	front := httptest.NewUnstartedServer(srv.buildRouter())
	front.EnableHTTP2 = true
	front.StartTLS()
	t.Cleanup(front.Close)

	cases := []struct {
		name, authority, path string
		wantStatus            int
		wantBody              string
		wantRequests          int64
	}{
		{"valid public virtual host", "public.example", "/", http.StatusOK, "public", 1},
		{"valid Unicode virtual host", "bücher.example", "/", http.StatusOK, "unicode", 2},
		{"invalid authority", "bad host", "/", http.StatusBadRequest, "", 2},
		{"invalid authority across retry", "bad host", "/retry/x", http.StatusBadRequest, "", 2},
		{"shared pool remains usable", "public.example", "/retry/x", http.StatusOK, "public", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := rawHTTP2AuthorityRequest(t, front.Listener.Addr().String(), tc.authority, tc.path)
			if status != tc.wantStatus {
				t.Errorf("status: got %d, want %d; body=%q", status, tc.wantStatus, body)
			}
			if tc.wantBody != "" && !strings.Contains(body, tc.wantBody) {
				t.Errorf("body %q does not contain %q", body, tc.wantBody)
			}
			if got := backendRequests.Load(); got != tc.wantRequests {
				t.Errorf("backend requests: got %d, want %d", got, tc.wantRequests)
			}
			if ph.passive.Load().demoted(ph.primary[0]) {
				t.Error("request Host rejection demoted the backend")
			}
		})
	}
}

func clientHostVirtualHosts(requests *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.Host {
		case "public.example":
			_, _ = io.WriteString(w, "public")
		case "xn--bcher-kva.example":
			_, _ = io.WriteString(w, "unicode")
		case "":
			_, _ = io.WriteString(w, "sensitive-default")
		default:
			http.Error(w, "unknown virtual host", http.StatusMisdirectedRequest)
		}
	})
}

func TestClientHostRejectsEmptyHostBeforeBackendSelection(t *testing.T) {
	var backendRequests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	srv, err := newServer(mustResolve(t, hostPoolConfig(backend.URL, ClientHost)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.pools["api"].transport.CloseIdleConnections() })

	req := httptest.NewRequest(http.MethodGet, "http://placeholder/x", nil)
	req.Host = ""
	rec := runRequest(t, srv.buildRouter(), req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
	if got := backendRequests.Load(); got != 0 {
		t.Errorf("backend requests: got %d, want 0", got)
	}
}

func TestHostOverridesDoNotRequireUsableClientHost(t *testing.T) {
	backend := newEchoBackend(t)
	for _, tc := range []struct {
		name   string
		policy UpstreamHost
		want   func(string) string
	}{
		{"target host", TargetHost, func(target string) string { return target }},
		{"explicit host", HostValue("public.internal"), func(string) string { return "public.internal" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := newServer(mustResolve(t, hostPoolConfig(backend.URL, tc.policy)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { srv.pools["api"].transport.CloseIdleConnections() })

			req := httptest.NewRequest(http.MethodGet, "http://placeholder/x", nil)
			req.Host = "bad host"
			rec := runRequest(t, srv.buildRouter(), req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200", rec.Code)
			}
			target := strings.TrimPrefix(backend.URL, "http://")
			if got, want := decodeEcho(t, rec.Body).Host, tc.want(target); got != want {
				t.Errorf("upstream Host: got %q, want %q", got, want)
			}
		})
	}
}

func rawHTTP2AuthorityRequest(t *testing.T, addr, authority, path string) (int, string) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test-only ephemeral certificate
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if state := conn.ConnectionState(); state.NegotiatedProtocol != "h2" {
		t.Fatalf("negotiated protocol %q, want h2", state.NegotiatedProtocol)
	}
	framer := http2.NewFramer(conn, conn)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	writeHTTP2AuthorityRequest(t, conn, framer, authority, path)
	return readHTTP2AuthorityResponse(t, framer)
}

func writeHTTP2AuthorityRequest(t *testing.T, conn io.Writer, framer *http2.Framer, authority, path string) {
	t.Helper()
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	if err := framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: http.MethodGet},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: authority},
		{Name: ":path", Value: path},
	} {
		if err := encoder.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: block.Bytes(),
		EndHeaders:    true,
		EndStream:     true,
	}); err != nil {
		t.Fatal(err)
	}
}

func readHTTP2AuthorityResponse(t *testing.T, framer *http2.Framer) (int, string) {
	t.Helper()
	status := 0
	var body bytes.Buffer
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if processHTTP2AuthorityResponse(t, framer, frame, &status, &body) {
			return status, body.String()
		}
	}
}

func processHTTP2AuthorityResponse(t *testing.T, framer *http2.Framer, frame http2.Frame, status *int, body *bytes.Buffer) bool {
	t.Helper()
	if id := frame.Header().StreamID; id != 0 && id != 1 {
		return false
	}
	switch frame := frame.(type) {
	case *http2.SettingsFrame:
		ackHTTP2AuthoritySettings(t, framer, frame)
	case *http2.MetaHeadersFrame:
		*status = http2AuthorityResponseStatus(t, frame, *status)
		return frame.StreamEnded()
	case *http2.DataFrame:
		_, _ = body.Write(frame.Data())
		return frame.StreamEnded()
	case *http2.RSTStreamFrame:
		t.Fatalf("stream reset: %s", frame.ErrCode)
	case *http2.GoAwayFrame:
		t.Fatalf("connection closed: %s", frame.ErrCode)
	}
	return false
}

func ackHTTP2AuthoritySettings(t *testing.T, framer *http2.Framer, frame *http2.SettingsFrame) {
	t.Helper()
	if !frame.IsAck() {
		if err := framer.WriteSettingsAck(); err != nil {
			t.Fatal(err)
		}
	}
}

func http2AuthorityResponseStatus(t *testing.T, frame *http2.MetaHeadersFrame, status int) int {
	t.Helper()
	for _, field := range frame.Fields {
		if field.Name == ":status" {
			value, err := strconv.Atoi(field.Value)
			if err != nil {
				t.Fatalf("invalid response status %q", field.Value)
			}
			return value
		}
	}
	return status
}
