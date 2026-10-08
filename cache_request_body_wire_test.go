package statute

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/quicvarint"
)

func TestCacheEmptyRequestWire(t *testing.T) {
	for _, proto := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("HTTP%d", proto), func(t *testing.T) {
			var calls atomic.Int64
			tcpAddr, udpAddr := startCacheBodyWireServer(t, cacheBodyWireOrigin(&calls))
			client, addr := cacheBodyWireClient(t, proto, tcpAddr, udpAddr)
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(method, func(t *testing.T) {
					base := calls.Load()
					for attempt := int64(1); attempt <= 3; attempt++ {
						wantCalls, wantLength := base+1, int64(0)
						if proto == 3 {
							// quic-go leaves absent Content-Length unknown even after
							// an empty request stream ends. C10 deliberately bypasses it.
							wantCalls, wantLength = base+attempt, -1
						}
						h := cacheBodyWireRequest(t, client, addr, method, "/"+method, "", false, proto)
						assertCacheBodyWireOrigin(t, h, wantCalls, wantLength, "")
						if got := calls.Load(); got != wantCalls {
							t.Fatalf("origin calls = %d, want %d", got, wantCalls)
						}
					}
				})
			}
		})
	}
}

func TestCacheRequestBodyWireBypass(t *testing.T) {
	for _, proto := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("HTTP%d", proto), func(t *testing.T) {
			var calls atomic.Int64
			tcpAddr, udpAddr := startCacheBodyWireServer(t, cacheBodyWireOrigin(&calls))
			client, addr := cacheBodyWireClient(t, proto, tcpAddr, udpAddr)
			// The TCP and QUIC listeners share the route's cache. Empty HTTP/2
			// requests can seed and check an entry even when HTTP/3 cannot.
			emptyClient, emptyAddr := cacheBodyWireClient(t, 2, tcpAddr, udpAddr)
			scenario := cacheBodyWireScenario{client, emptyClient, addr, emptyAddr, &calls, proto}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				for _, unknown := range []bool{false, true} {
					for _, warm := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/unknown=%v/warm=%v", method, unknown, warm), func(t *testing.T) {
							scenario.check(t, method, unknown, warm)
						})
					}
				}
			}
		})
	}
}

type cacheBodyWireScenario struct {
	client, emptyClient *http.Client
	addr, emptyAddr     string
	calls               *atomic.Int64
	proto               int
}

func (s cacheBodyWireScenario) check(t *testing.T, method string, unknown, warm bool) {
	t.Helper()
	path := fmt.Sprintf("/%s/%v/%v", method, unknown, warm)
	base := s.calls.Load()
	wantCalls := base
	if warm {
		wantCalls++
		for range 2 {
			h := cacheBodyWireRequest(t, s.emptyClient, s.emptyAddr, method, path, "", false, 2)
			assertCacheBodyWireOrigin(t, h, wantCalls, 0, "")
		}
	}
	for _, payload := range []string{"first-body", "second-body"} {
		wantCalls++
		wantLength := int64(len(payload))
		if unknown {
			wantLength = -1
		}
		h := cacheBodyWireRequest(t, s.client, s.addr, method, path, payload, unknown, s.proto)
		assertCacheBodyWireOrigin(t, h, wantCalls, wantLength, payload)
	}
	// Body responses cannot replace the warm entry or populate a cold one.
	cachedCall := base + 1
	if !warm {
		wantCalls++
		cachedCall = wantCalls
	}
	for range 2 {
		h := cacheBodyWireRequest(t, s.emptyClient, s.emptyAddr, method, path, "", false, 2)
		assertCacheBodyWireOrigin(t, h, cachedCall, 0, "")
	}
	if got := s.calls.Load(); got != wantCalls {
		t.Fatalf("origin calls = %d, want %d", got, wantCalls)
	}
}

func cacheBodyWireOrigin(calls *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Origin-Call", strconv.FormatInt(call, 10))
		w.Header().Set("X-Request-Length", strconv.FormatInt(r.ContentLength, 10))
		w.Header().Set("X-Request-Body", string(body))
		_, _ = fmt.Fprintf(w, "origin-%d:%s", call, body)
	})
}

func startCacheBodyWireServer(t *testing.T, origin http.Handler) (string, string) {
	t.Helper()
	certFile, keyFile := writeSelfSignedCert(t, "h3.example")
	tcpAddr, udpAddr := reserveAddr(t), reserveUDPAddr(t)
	srv, err := newServer(mustResolve(t, Config{
		Listeners: Listeners{HTTPS(tcpAddr, StaticTLS(certFile, keyFile), HTTP3(udpAddr))},
		Routes:    Routes{Match("/*").Handle(origin).With(Cache("1h"))},
		Shutdown:  Shutdown{GracePeriod: "2s"},
	}))
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
	return tcpAddr, udpAddr
}

func cacheBodyWireClient(t *testing.T, proto int, tcpAddr, udpAddr string) (*http.Client, string) {
	t.Helper()
	tlsConfig := &tls.Config{
		ServerName: "h3.example", InsecureSkipVerify: true, //nolint:gosec // Hermetic self-signed server.
	}
	if proto == 3 {
		tr := &http3.Transport{TLSClientConfig: tlsConfig}
		t.Cleanup(func() { _ = tr.Close() })
		return &http.Client{Transport: tr, Timeout: 5 * time.Second}, udpAddr
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(proto == 1)
	protocols.SetHTTP2(proto == 2)
	tr := &http.Transport{TLSClientConfig: tlsConfig, Protocols: protocols}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}, tcpAddr
}

func cacheBodyWireRequest(t *testing.T, client *http.Client, addr, method, path, payload string, unknown bool, proto int) http.Header {
	t.Helper()
	var body io.Reader
	if payload != "" {
		body = strings.NewReader(payload)
		if unknown {
			body = io.NopCloser(body) // Hide the length from http.NewRequest.
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "https://"+addr+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "h3.example"
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return cacheBodyWireResponse(t, res, method, payload, proto)
}

func cacheBodyWireResponse(t *testing.T, res *http.Response, method, payload string, proto int) http.Header {
	t.Helper()
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK || res.ProtoMajor != proto {
		t.Fatalf("response: protocol=%s status=%d body=%q error=%v", res.Proto, res.StatusCode, data, err)
	}
	if method == http.MethodHead && len(data) != 0 {
		t.Fatalf("HEAD response body = %q", data)
	}
	if method == http.MethodGet {
		want := "origin-" + res.Header.Get("X-Origin-Call") + ":" + payload
		if string(data) != want {
			t.Fatalf("response body = %q, want %q", data, want)
		}
	}
	return res.Header
}

func assertCacheBodyWireOrigin(t *testing.T, h http.Header, calls, length int64, payload string) {
	t.Helper()
	if h.Get("X-Origin-Call") != strconv.FormatInt(calls, 10) ||
		h.Get("X-Request-Length") != strconv.FormatInt(length, 10) || h.Get("X-Request-Body") != payload {
		t.Fatalf("origin headers = %v, want call=%d length=%d body=%q", h, calls, length, payload)
	}
}

func TestCacheHTTP3ExplicitZeroLengthWire(t *testing.T) {
	var calls atomic.Int64
	_, udpAddr := startCacheBodyWireServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 3 || r.ContentLength != 0 {
			t.Errorf("origin request protocol=%s length=%d, want HTTP/3 length=0", r.Proto, r.ContentLength)
		}
		// Observe FIN before responding: quic-go cancels an unread request
		// stream when the handler returns, which can race the client's Close.
		if n, err := io.Copy(io.Discard, r.Body); err != nil || n != 0 {
			t.Errorf("empty request body: bytes=%d error=%v", n, err)
		}
		_, _ = fmt.Fprintf(w, "origin-%d", calls.Add(1))
	}))
	conn := cacheHTTP3Conn(t, udpAddr)
	for i := range 3 {
		want := fmt.Sprintf("origin-%d", i+1)
		if body := cacheHTTP3ExplicitZeroRequest(t, conn); body != want {
			t.Fatalf("explicit Content-Length: 0 response = %q, want %q", body, want)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("origin calls = %d, want 3", got)
	}
}

func TestCacheHTTP3UnannouncedTrailerIsolation(t *testing.T) {
	var calls atomic.Int64
	_, addr := startCacheBodyWireServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "%d:%s", calls.Add(1), html.EscapeString(r.Trailer.Get("X-Selection")))
	}))
	conn := cacheHTTP3Conn(t, addr)
	for i, selection := range []string{"first", "second", ""} {
		got := cacheHTTP3ExplicitZeroRequest(t, conn, selection)
		want := fmt.Sprintf("%d:%s", i+1, selection)
		if got != want {
			t.Fatalf("trailer=%q: got %q want %q", selection, got, want)
		}
	}
}

func cacheHTTP3Conn(t *testing.T, udpAddr string) *quic.Conn {
	t.Helper()
	conn, err := quic.DialAddr(t.Context(), udpAddr, &tls.Config{
		ServerName: "h3.example", NextProtos: []string{http3.NextProtoH3},
		InsecureSkipVerify: true, //nolint:gosec // Hermetic self-signed server.
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseWithError(0, "") })
	transport := &http3.Transport{}
	// Let the normal HTTP/3 client own control streams and peer settings.
	_ = transport.NewClientConn(conn)
	t.Cleanup(func() { _ = transport.Close() })
	return conn
}

func cacheHTTP3ExplicitZeroRequest(t *testing.T, conn *quic.Conn, trailer ...string) string {
	t.Helper()
	stream, err := conn.OpenStreamSync(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// The regular quic-go GET writer omits zero length. Send static QPACK
	// fields directly so this request explicitly declares Content-Length: 0.
	headers := append([]byte{0, 0, 0xd1, 0xd7, 0xc1, 0x50, 10}, "h3.example"...)
	headers = append(headers, 0xc4)
	cacheHTTP3WriteHeaders(t, stream, headers)
	if len(trailer) != 0 && trailer[0] != "" {
		// Literal name x-selection (11 bytes), without a dynamic table.
		block := append([]byte{0, 0, 0x27, 4}, "x-selection"...)
		if len(trailer[0]) > 127 {
			t.Fatal("test trailer too long")
		}
		block = append(block, byte(len(trailer[0])&127))
		block = append(block, trailer[0]...)
		cacheHTTP3WriteHeaders(t, stream, block)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	return cacheHTTP3ReadData(t, response)
}

func cacheHTTP3WriteHeaders(t *testing.T, stream *quic.Stream, block []byte) {
	t.Helper()
	frame := quicvarint.Append(nil, 1)
	frame = quicvarint.Append(frame, uint64(len(block)))
	frame = append(frame, block...)
	if _, err := stream.Write(frame); err != nil {
		t.Fatal(err)
	}
}

func cacheHTTP3ReadData(t *testing.T, response []byte) string {
	t.Helper()
	reader := bytes.NewReader(response)
	var body bytes.Buffer
	for reader.Len() > 0 {
		kind, err := quicvarint.Read(reader)
		if err != nil {
			t.Fatal(err)
		}
		length, err := quicvarint.Read(reader)
		if err != nil || length > 1<<20 {
			t.Fatalf("invalid HTTP/3 response frame: length=%d remaining=%d error=%v", length, reader.Len(), err)
		}
		dst := io.Discard
		if kind == 0 { // HTTP/3 DATA frame.
			dst = &body
		}
		if _, err := io.CopyN(dst, reader, int64(length&0x1fffff)); err != nil {
			t.Fatal(err)
		}
	}
	return body.String()
}
