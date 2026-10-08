package statute

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/quic-go/qpack"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/quicvarint"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestRequestTrailerHTTP3FrameworkClones(t *testing.T) {
	for _, tc := range []struct {
		name       string
		middleware []Middleware
	}{
		{"listener_context", nil},
		{"timeout", []Middleware{Timeout("5s")}},
		{"etag", []Middleware{ETag()}},
		{"timeout_etag_body_limit", []Middleware{Timeout("5s"), ETag(), BodyLimit("4KiB")}},
		{"path_rewrite", []Middleware{ReplacePath("/rewritten"), ETag()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantPath := "/"
			if tc.name == "path_rewrite" {
				wantPath = "/rewritten"
			}
			origin := requestTrailerIncomingReport(t, wantPath)
			conn := requestTrailerServer(t, Config{Routes: Routes{Match("/*").Handle(origin).With(tc.middleware...)}})
			for _, payload := range []string{"", "ab"} {
				for _, declared := range []bool{false, true} {
					for _, knownLength := range []bool{false, true} {
						t.Run(fmt.Sprintf("body=%q/declared=%v/length=%v", payload, declared, knownLength), func(t *testing.T) {
							length := int64(-1)
							if knownLength {
								length = int64(len(payload))
							}
							got := requestTrailerHTTP3(t, conn, payload, declared, knownLength)
							want := fmt.Sprintf("%d:%s:selection", length, payload)
							if got != want {
								t.Fatalf("report=%q want=%q", got, want)
							}
						})
					}
				}
			}
		})
	}
}

func requestTrailerIncomingReport(t *testing.T, wantPath string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beforeLength := r.ContentLength
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read incoming body: %v", err)
			return
		}
		if r.ContentLength != beforeLength || len(r.TransferEncoding) != 0 || r.URL.Path != wantPath || r.RequestURI != "/" {
			t.Errorf("incoming request changed: length=%d before=%d encoding=%v path=%q URI=%q", r.ContentLength, beforeLength, r.TransferEncoding, r.URL.Path, r.RequestURI)
		}
		report := fmt.Sprintf("%d:%s:%s", beforeLength, body, r.Trailer.Get("X-Selection"))
		_, _ = io.WriteString(w, html.EscapeString(report))
	})
}

func TestRequestTrailerHTTP3NativeUpstreams(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("HTTP2=%v", h2), func(t *testing.T) {
			origin := requestTrailerUpstreamOrigin(t, h2)
			rawURL := origin.URL
			if h2 {
				rawURL = requestTrailerRawHTTP2Origin(t).URL
			}
			for _, tc := range []struct {
				name       string
				middleware []Middleware
			}{
				{"plain", nil},
				{"timeout_etag", []Middleware{Timeout("5s"), ETag()}},
				{"body_limit", []Middleware{BodyLimit("4KiB")}},
				{"retry_buffered", []Middleware{Retry(2).RequestBufferBudget("4KiB")}},
				{"retry_fallback", []Middleware{Retry(2).RequestBufferBudget("1B")}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					conn := requestTrailerServer(t, Config{
						Upstreams: Upstreams{
							"origin": Pool{Backends: []Backend{{Address: origin.URL}}, Transport: Transport{InsecureSkipVerify: h2}},
							"raw":    Pool{Backends: []Backend{{Address: rawURL}}, Transport: Transport{InsecureSkipVerify: h2}},
						},
						Routes: Routes{Match("/raw").ProxyTo("raw").With(tc.middleware...), Match("/*").ProxyTo("origin").With(tc.middleware...)},
					})
					for _, payload := range []string{"", "ab"} {
						for _, declared := range []bool{false, true} {
							for _, knownLength := range []bool{false, true} {
								t.Run(fmt.Sprintf("body=%q/declared=%v/length=%v", payload, declared, knownLength), func(t *testing.T) {
									path := "/"
									if h2 && !declared {
										path = "/raw"
									}
									got := requestTrailerHTTP3(t, conn, payload, declared, knownLength, path)
									if want := payload + ":selection"; got != want {
										t.Fatalf("upstream report=%q want=%q", got, want)
									}
								})
							}
						}
					}
				})
			}
		})
	}
}

func requestTrailerUpstreamOrigin(t *testing.T, h2 bool) *httptest.Server {
	t.Helper()
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		wantProto := 1
		if h2 {
			wantProto = 2
		}
		if r.ProtoMajor != wantProto {
			t.Errorf("upstream protocol=%s want HTTP/%d", r.Proto, wantProto)
		}
		if r.Header.Get("X-Selection") != "" {
			t.Error("trailer became an ordinary header")
		}
		_, _ = fmt.Fprintf(w, "%s:%s", html.EscapeString(string(body)), html.EscapeString(r.Trailer.Get("X-Selection")))
	}))
	origin.EnableHTTP2 = h2
	if h2 {
		origin.StartTLS()
	} else {
		origin.Start()
	}
	t.Cleanup(origin.Close)
	return origin
}

// Use the actual shared listener handler, including its first context clone.
func requestTrailerServer(t *testing.T, cfg Config) *quic.Conn {
	t.Helper()
	cert, key := writeSelfSignedCert(t, "h3.example")
	cfg.Listeners = Listeners{HTTPS("127.0.0.1:0", StaticTLS(cert, key), HTTP3("127.0.0.1:0"), TrustedProxy("127.0.0.0/8"))}
	cfg.Shutdown = Shutdown{GracePeriod: "2s"}
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
	return cacheHTTP3Conn(t, srv.run.listeners.http3[0].conn.LocalAddr().String())
}

func requestTrailerHTTP3(t *testing.T, conn *quic.Conn, payload string, declared, knownLength bool, paths ...string) string {
	t.Helper()
	stream, err := conn.OpenStreamSync(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	path := "/"
	if len(paths) != 0 {
		path = paths[0]
	}
	fields := []qpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: "h3.example"}, {Name: ":path", Value: path}}
	if declared {
		fields = append(fields, qpack.HeaderField{Name: "trailer", Value: "x-selection"})
	}
	if knownLength {
		fields = append(fields, qpack.HeaderField{Name: "content-length", Value: strconv.Itoa(len(payload))})
	}
	requestTrailerHeaders(t, stream, fields)
	if payload != "" {
		frame := quicvarint.Append(nil, 0)
		frame = quicvarint.Append(frame, uint64(len(payload)))
		frame = append(frame, payload...)
		if _, err := stream.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	requestTrailerHeaders(t, stream, []qpack.HeaderField{{Name: "x-selection", Value: "selection"}})
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	return cacheHTTP3ReadData(t, response)
}

func requestTrailerHeaders(t *testing.T, stream *quic.Stream, fields []qpack.HeaderField) {
	t.Helper()
	var block bytes.Buffer
	encoder := qpack.NewEncoder(&block)
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	cacheHTTP3WriteHeaders(t, stream, block.Bytes())
}

// Go's HTTP/2 server exposes only declared trailer names to handlers. Inspect
// trailing HEADERS directly to prove unannounced fields survive on the wire.
func requestTrailerRawHTTP2Origin(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("raw HTTP/2 origin received an HTTP/1 request")
		w.WriteHeader(http.StatusHTTPVersionNotSupported)
	}))
	srv.EnableHTTP2 = true
	srv.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		"h2": func(_ *http.Server, conn *tls.Conn, _ http.Handler) { requestTrailerReadHTTP2(t, conn) },
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

type requestTrailerHTTP2Stream struct {
	body      bytes.Buffer
	selection string
	headers   bool
}

func requestTrailerReadHTTP2(t *testing.T, conn *tls.Conn) {
	t.Helper()
	defer conn.Close()
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil {
		return
	}
	if string(preface) != http2.ClientPreface {
		t.Error("invalid HTTP/2 client preface")
		return
	}
	framer := http2.NewFramer(conn, conn)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := framer.WriteSettings(); err != nil {
		return
	}
	streams := make(map[uint32]*requestTrailerHTTP2Stream)
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return
		}
		id := frame.Header().StreamID
		ended, err := requestTrailerProcessHTTP2Frame(t, framer, streams, frame)
		if err != nil {
			return
		}
		if ended {
			if err := requestTrailerReplyHTTP2(framer, id, streams[id]); err != nil {
				return
			}
			delete(streams, id)
		}
	}
}

func requestTrailerProcessHTTP2Frame(t *testing.T, framer *http2.Framer, streams map[uint32]*requestTrailerHTTP2Stream, frame http2.Frame) (bool, error) {
	t.Helper()
	id := frame.Header().StreamID
	switch frame := frame.(type) {
	case *http2.SettingsFrame:
		if !frame.IsAck() {
			return false, framer.WriteSettingsAck()
		}
	case *http2.PingFrame:
		if !frame.Flags.Has(http2.FlagPingAck) {
			return false, framer.WritePing(true, frame.Data)
		}
	case *http2.MetaHeadersFrame:
		requestTrailerRecordHTTP2Headers(t, streams, frame)
		return frame.StreamEnded(), nil
	case *http2.DataFrame:
		stream := streams[id]
		if stream == nil {
			t.Error("HTTP/2 DATA without request headers")
			return false, io.ErrUnexpectedEOF
		}
		_, _ = stream.body.Write(frame.Data())
		return frame.StreamEnded(), nil
	case *http2.RSTStreamFrame:
		delete(streams, id)
	}
	return false, nil
}

func requestTrailerRecordHTTP2Headers(t *testing.T, streams map[uint32]*requestTrailerHTTP2Stream, frame *http2.MetaHeadersFrame) {
	t.Helper()
	id := frame.Header().StreamID
	stream := streams[id]
	if stream == nil {
		stream = &requestTrailerHTTP2Stream{}
		streams[id] = stream
	}
	for _, field := range frame.Fields {
		if field.Name == "x-selection" {
			if !stream.headers {
				t.Error("trailer became an initial HTTP/2 header")
			}
			stream.selection = field.Value
		}
	}
	stream.headers = true
}

func requestTrailerReplyHTTP2(framer *http2.Framer, id uint32, stream *requestTrailerHTTP2Stream) error {
	var headers bytes.Buffer
	encoder := hpack.NewEncoder(&headers)
	if err := encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"}); err != nil {
		return err
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: headers.Bytes(), EndHeaders: true}); err != nil {
		return err
	}
	return framer.WriteData(id, true, []byte(stream.body.String()+":"+stream.selection))
}
