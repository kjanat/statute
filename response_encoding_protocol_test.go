package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
)

func TestResponseEncodingPreservesPartialAndNoTransform(t *testing.T) {
	for _, coding := range []string{"br", "zstd"} {
		for _, mode := range []string{"partial status", "content range", "response no-transform", "request no-transform"} {
			t.Run(coding+"/"+mode, func(t *testing.T) {
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					switch mode {
					case "partial status":
						w.WriteHeader(http.StatusPartialContent)
					case "content range":
						w.Header().Set("Content-Range", "bytes 0-3/8")
					case "response no-transform":
						w.Header().Set("Cache-Control", "public, no-transform")
					}
					_, _ = io.WriteString(w, "body")
				}), EncodeResponse(EncodingPolicy{Encodings: []string{coding}, MinimumBytes: 1}))
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("Accept-Encoding", coding)
				if mode == "request no-transform" {
					r.Header.Set("Cache-Control", "no-transform")
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Header().Get("Content-Encoding") != "" || w.Body.String() != "body" {
					t.Fatalf("representation altered: %v %q", w.Header(), w.Body.String())
				}
			})
		}
	}
}

func TestResponseEncodingUpdatesValidatorsAndTrailers(t *testing.T) {
	for _, coding := range []string{"br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("ETag", `"identity"`)
				w.Header().Set("Content-Digest", "identity-digest")
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Trailer", "Content-Digest, X-Finished")
				_, _ = io.WriteString(w, "first")
				_, _ = io.WriteString(w, "second")
				w.Header().Set("Content-Digest", "late-identity-digest")
				w.Header().Set("X-Finished", "yes")
			}), EncodeResponse(EncodingPolicy{Encodings: []string{coding}, MinimumBytes: 1}))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept-Encoding", coding)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			res := w.Result()
			defer res.Body.Close()
			body, err := io.ReadAll(decodeEncodedResponse(t, coding, res.Body))
			if err != nil || string(body) != "firstsecond" {
				t.Fatalf("encoded body=%q err=%v", body, err)
			}
			if res.Header.Get("ETag") != `W/"identity"` || res.Header.Get("Content-Digest") != "" || res.Header.Get("Accept-Ranges") != "" || res.Trailer.Get("Content-Digest") != "" || res.Trailer.Get("X-Finished") != "yes" {
				t.Fatalf("representation metadata: headers=%v trailers=%v", res.Header, res.Trailer)
			}
		})
	}
}

func TestResponseEncodingPassesEarlyHintsBeforeFinalResponse(t *testing.T) {
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, strings.Repeat("body", 128))
	}), EncodeResponse(EncodingPolicy{Encodings: []string{"br"}, MinimumBytes: 1}))
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	seen := false
	r, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Accept-Encoding", "br")
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
		seen = code == http.StatusEarlyHints && h.Get("Link") != ""
		return nil
	}}))
	res, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(decodeEncodedResponse(t, "br", res.Body))
	if err != nil || !seen || res.StatusCode != http.StatusAccepted || string(body) != strings.Repeat("body", 128) {
		t.Fatalf("early hints=%t status=%d bytes=%d err=%v", seen, res.StatusCode, len(body), err)
	}
}
