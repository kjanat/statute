package statute

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"statute.kjanat.dev/resolved"
)

func TestResponseEncodingWildcardExclusions(t *testing.T) {
	for _, tc := range []struct{ accept, fallback, want string }{
		{"br;q=0, *;q=1", "", "zstd"},
		{"br;q=0, *;q=1", "br", "zstd"},
		{"br;q=0, zstd;q=0, *;q=1", "br", ""},
		{"br;q=0.2, *;q=1", "", "zstd"},
		{"br;q=1, *;q=0", "", "br"},
	} {
		t.Run(tc.accept+"/"+tc.fallback, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, strings.Repeat("body", 64))
			}), EncodeResponse(EncodingPolicy{Encodings: []string{"br", "zstd"}, DefaultEncoding: tc.fallback, MinimumBytes: 1}))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept-Encoding", tc.accept)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if got := w.Header().Get("Content-Encoding"); got != tc.want {
				t.Fatalf("encoding = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResponseEncodingDetectsContentType(t *testing.T) {
	body := "<!doctype html><html>" + strings.Repeat("hello", 200) + "</html>"
	for _, coding := range []string{"br", "zstd"} {
		for _, excluded := range []bool{false, true} {
			t.Run(coding+"/excluded="+map[bool]string{true: "yes", false: "no"}[excluded], func(t *testing.T) {
				policy := EncodingPolicy{Encodings: []string{coding}, MinimumBytes: 16}
				if excluded {
					policy.ExcludedTypes = []string{"text/html"}
				} else {
					policy.IncludedTypes = []string{"text/html"}
				}
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, body[:8])
					_, _ = io.WriteString(w, body[8:])
				}), EncodeResponse(policy))
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("Accept-Encoding", coding)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
					t.Fatalf("content type = %q", got)
				}
				actual := w.Header().Get("Content-Encoding")
				if excluded && actual != "" {
					t.Fatalf("excluded body encoded as %q", actual)
				}
				decoded := decodeEncodedResponse(t, actual, w.Body)
				got, err := io.ReadAll(decoded)
				if err != nil || string(got) != body {
					t.Fatalf("decoded body differs: %v", err)
				}
			})
		}
	}
}

func TestResponseEncodingFlushBeforeBody(t *testing.T) {
	for _, coding := range []string{"br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			w := httptest.NewRecorder()
			h := chain(t, http.HandlerFunc(func(out http.ResponseWriter, _ *http.Request) {
				out.Header().Set("Content-Type", "text/event-stream")
				out.WriteHeader(http.StatusAccepted)
				if err := http.NewResponseController(out).Flush(); err != nil {
					t.Fatal(err)
				}
				if !w.Flushed || w.Code != http.StatusAccepted {
					t.Fatalf("headers pending after Flush: flushed=%t status=%d", w.Flushed, w.Code)
				}
				_, _ = io.WriteString(out, "data: event\n\n")
			}), EncodeResponse(EncodingPolicy{Encodings: []string{coding}}))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept-Encoding", coding)
			h.ServeHTTP(w, r)
		})
	}
}

func TestResponseEncodingOwnsCodingHeader(t *testing.T) {
	for _, operation := range []Middleware{RemoveResponseHeader("Content-Encoding"), SetResponseHeader("Content-Encoding", "identity")} {
		for _, group := range []bool{false, true} {
			encoder := Middleware(EncodeResponse(EncodingPolicy{Encodings: []string{"br"}}))
			if group {
				encoder = InOrder(encoder)
			}
			_, err := Resolve(Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(encoder, operation)}})
			if err == nil {
				t.Fatal("accepted conflicting Content-Encoding writer")
			}
		}
	}
}

func decodeEncodedResponse(t *testing.T, coding string, body io.Reader) io.Reader {
	t.Helper()
	switch coding {
	case "br":
		return brotli.NewReader(body)
	case "zstd":
		reader, err := zstd.NewReader(body)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(reader.Close)
		return reader
	default:
		return body
	}
}

func TestResponseEncodingDefaultAndSmallBodies(t *testing.T) {
	for _, tc := range []struct {
		name, accept string
		policy       EncodingPolicy
		method       string
		status       int
		want         string
	}{
		{"default brotli", "", EncodingPolicy{Encodings: []string{"br"}, DefaultEncoding: "br", MinimumBytes: 1}, "GET", 200, "br"},
		{"default gzip", "", EncodingPolicy{Encodings: []string{"gzip"}, DefaultEncoding: "gzip", MinimumBytes: 1}, "GET", 200, "gzip"},
		{"empty header", "", EncodingPolicy{Encodings: []string{"br"}, DefaultEncoding: "br", MinimumBytes: 1}, "GET", 200, ""},
		{"head", "br", EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1}, "HEAD", 200, ""},
		{"no content", "br", EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1}, "GET", 204, ""},
		{"reset content", "br", EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1}, "GET", 205, ""},
		{"not modified", "br", EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1}, "GET", 304, ""},
		{"unknown coding", "deflate", EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1}, "GET", 200, ""},
		{"malformed quality", "br;q=bogus", EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1}, "GET", 200, ""},
		{"identity preferred", "identity;q=1, br;q=0.5", EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1}, "GET", 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "small body")
			}), EncodeResponse(tc.policy))
			r := httptest.NewRequest(tc.method, "/", nil)
			if tc.accept != "" || tc.name == "empty header" {
				r.Header.Set("Accept-Encoding", tc.accept)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Content-Encoding") != tc.want {
				t.Fatalf("status=%d encoding=%q", w.Code, w.Header().Get("Content-Encoding"))
			}
		})
	}
}

func TestResponseEncodingValidation(t *testing.T) {
	for _, policy := range []EncodingPolicy{
		{Encodings: []string{"unsupported"}}, {Encodings: []string{"br"}, DefaultEncoding: "gzip"}, {MinimumBytes: -1}, {IncludedTypes: []string{"text/html"}, ExcludedTypes: []string{"text/plain"}}, {IncludedTypes: []string{"invalid content type"}},
	} {
		if _, err := resolveMiddleware(EncodeResponse(policy)); err == nil {
			t.Fatal("invalid encoding policy accepted")
		}
	}
	for _, mw := range []resolved.Middleware{{}, {Encoding: &resolved.EncodingPolicy{MinimumBytes: -1}}} {
		w := httptest.NewRecorder()
		encodeResponseHandler(mw, http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("invalid resolved policy status=%d", w.Code)
		}
	}
}

func TestResponseEncodingFlushesBufferedPrefixAndLaterWrites(t *testing.T) {
	for _, coding := range []string{"br", "zstd"} {
		w := httptest.NewRecorder()
		h := chain(t, http.HandlerFunc(func(out http.ResponseWriter, _ *http.Request) {
			out.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(out, "first")
			if err := http.NewResponseController(out).Flush(); err != nil {
				t.Fatal(err)
			}
			if !w.Flushed {
				t.Fatal("buffered prefix did not flush")
			}
			_, _ = io.WriteString(out, "second")
		}), EncodeResponse(EncodingPolicy{Encodings: []string{coding}, MinimumBytes: 64}))
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept-Encoding", coding)
		h.ServeHTTP(w, r)
		got, err := io.ReadAll(decodeEncodedResponse(t, w.Header().Get("Content-Encoding"), w.Body))
		if err != nil || string(got) != "firstsecond" {
			t.Fatalf("stream body=%q err=%v", got, err)
		}
	}
}

func TestResponseEncodingLeavesExistingAndInvalidMediaTypes(t *testing.T) {
	for _, kind := range []string{"invalid content type", "application/grpc", "text/plain"} {
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", kind)
			if kind == "text/plain" {
				w.Header().Set("Content-Encoding", "identity")
			}
			_, _ = io.WriteString(w, "first")
			_, _ = io.WriteString(w, "second")
		}), EncodeResponse(EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1}))
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept-Encoding", "br")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := ""
		if kind == "text/plain" {
			want = "identity"
		}
		if w.Header().Get("Content-Encoding") != want || w.Body.String() != "firstsecond" {
			t.Fatalf("existing representation altered: %s %q", kind, w.Body.String())
		}
	}
}

func TestResponseEncodingPreservesUpgradeAndDeadlineInterfaces(t *testing.T) {
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		controller := http.NewResponseController(w)
		if err := controller.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			t.Error(err)
			return
		}
		conn, rw, err := controller.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\nupgraded")
		_ = rw.Flush()
	}), EncodeResponse(EncodingPolicy{Encodings: []string{"br"}}))
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.test\r\nAccept-Encoding: br\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Content-Encoding") != "" {
		t.Fatalf("upgrade altered: %d %v", response.StatusCode, response.Header)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "upgraded" {
		t.Fatalf("upgrade bytes=%q err=%v", data, err)
	}
}

func TestResponseEncodingBypassesMalformedAndExcludedRequests(t *testing.T) {
	for _, tc := range []struct{ name, accept, requestType string }{
		{"malformed accept", "br;q=bogus", ""},
		{"grpc request", "br", "application/grpc"},
		{"excluded request", "br", "text/csv"},
		{"eligible", "br", "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, strings.Repeat("body", 64))
			}), EncodeResponse(EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1, ExcludedTypes: []string{"text/csv"}}))
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.Header.Set("Accept-Encoding", tc.accept)
			if tc.requestType != "" {
				r.Header.Set("Content-Type", tc.requestType)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := ""
			if tc.name == "eligible" {
				want = "br"
			}
			if vary := w.Header().Values("Vary"); w.Header().Get("Content-Encoding") != want || len(vary) != 1 || vary[0] != "Accept-Encoding" {
				t.Fatalf("encoding=%q vary=%q", w.Header().Get("Content-Encoding"), w.Header().Values("Vary"))
			}
		})
	}
}
