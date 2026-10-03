package statute

import (
	"bytes"
	"compress/gzip"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

func TestCompressionNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name, accept, origin, coding string
		absent                       bool
		status                       int
	}{
		{name: "absent", absent: true},
		{name: "empty"},
		{name: "gzip", accept: "gzip;q=1", coding: "gzip"},
		{name: "excluded gzip", accept: "gzip;q=0"},
		{name: "no acceptable coding", accept: "gzip;q=0, identity;q=0", status: 406},
		{name: "wildcard excludes identity", accept: "*;q=0", status: 406},
		{name: "explicit identity", accept: "*;q=0, identity;q=1"},
		{name: "weighted", accept: "br;q=.5, gzip;q=1", coding: "gzip"},
		{name: "origin zstd", accept: "zstd, identity;q=0", origin: "zstd", coding: "zstd"},
		{name: "cannot generate zstd", accept: "zstd, identity;q=0", status: 406},
		{name: "case insensitive", accept: "GZip; Q=0.8, BR;q=0.7", coding: "gzip"},
		{name: "token not substring", accept: "notgzip, zebra"},
		{name: "duplicate excludes", accept: "gzip, GZIP;q=0, identity;q=0", status: 406},
		{name: "reversed duplicate excludes", accept: "GZIP;q=0, gzip, identity;q=0", status: 406},
		{name: "wildcard respects specific zero", accept: "gzip;q=0, *;q=0.5", coding: "br"},
		{name: "wildcard tie", accept: "*;q=1", coding: "br"},
		{name: "identity preferred", accept: "gzip;q=.5, identity;q=1"},
		{name: "whitespace", accept: " , \t GZIP ; q=0.5 , ", coding: "gzip"},
		{name: "origin acceptable despite lower weight", accept: "zstd;q=.1,gzip", origin: "zstd", coding: "zstd"},
		{name: "origin forbidden", accept: "gzip, zstd;q=0", origin: "zstd", status: 406},
		{name: "origin any without preferences", absent: true, origin: "zstd", coding: "zstd"},
		{name: "origin encoded empty preferences", origin: "gzip", status: 406},
		{name: "accepted stack", accept: "gzip,br", origin: "gzip, br", coding: "gzip, br"},
		{name: "rejected stack", accept: "gzip,br;q=0", origin: "gzip, br", status: 406},
		{name: "invalid stack", accept: "*", origin: "gzip,", status: 406},
		{name: "legacy alias", accept: "x-gzip", coding: "gzip"},
		{name: "legacy alias exclusion", accept: "x-gzip,gzip;q=0,identity;q=0", status: 406},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.origin != "" {
					w.Header()["content-encoding"] = []string{tc.origin}
				}
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, "body")
			})
			req := httptest.NewRequest("GET", "/", nil)
			if !tc.absent {
				req.Header.Set("Accept-Encoding", tc.accept)
			}
			res := runRequest(t, chain(t, base, Compress(Gzip, Brotli)), req)
			status := tc.status
			if status == 0 {
				status = 200
			}
			values, _ := cacheHeaderValues(res.Header(), "Content-Encoding")
			if res.Code != status || strings.Join(values, ",") != tc.coding {
				t.Fatalf("got status=%d headers=%v; want status=%d coding=%q", res.Code, res.Header(), status, tc.coding)
			}
			assertNegotiatedBody(t, res, tc.coding, tc.origin != "")
		})
	}
}

func assertNegotiatedBody(t *testing.T, res *httptest.ResponseRecorder, coding string, passthrough bool) {
	t.Helper()
	if !strings.Contains(strings.Join(res.Header().Values("Vary"), ","), "Accept-Encoding") {
		t.Fatal("missing encoding variance")
	}
	if res.Code == 406 {
		if res.Body.Len() != 0 || res.Header().Get("Content-Length") != "0" {
			t.Fatal("406 must be empty")
		}
		return
	}
	var reader io.Reader = bytes.NewReader(res.Body.Bytes())
	if !passthrough {
		reader = compressionBodyReader(t, reader, coding)
	}
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != "body" {
		t.Fatalf("body=%q error=%v", body, err)
	}
}

func compressionBodyReader(t *testing.T, reader io.Reader, coding string) io.Reader {
	t.Helper()
	switch coding {
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = gz.Close() })
		return gz
	case "br":
		return brotli.NewReader(reader)
	default:
		return reader
	}
}

func TestAcceptEncodingMalformed(t *testing.T) {
	for _, value := range []string{"gzip;q=.", "gzip;q=", "gzip;q=1.1", "gzip;q=-1", "gzip;q=2", "gzip;q=NaN", "gzip;q=1e0", "gzip;q=0.0001", "gzip;q=\"0.5\"", "gzip;q=0.5;q=0", "gzip;level=1", "g zip", "gzip;", "gzip;q=0.5\n"} {
		t.Run(value, func(t *testing.T) {
			base := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid preferences reached origin") })
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Accept-Encoding", value)
			res := runRequest(t, chain(t, base, Compress(Gzip, Brotli)), req)
			if res.Code != 400 || res.Body.Len() != 0 {
				t.Fatalf("malformed header got %d %q", res.Code, res.Body.String())
			}
		})
	}
}

func TestAcceptEncodingRepeatedFields(t *testing.T) {
	h := http.Header{"Accept-Encoding": {"gzip;q=.5", "br;q=0.6"}, "accept-encoding": {"GZIP;q=0"}}
	p, ok := parseAcceptEncoding(h)
	if !ok || p.weight("gzip") != 0 || p.weight("BR") != 600 || p.weight("IDENTITY") != 1000 {
		t.Fatalf("repeated fields lost preferences: %+v", p)
	}
	for value, want := range map[string]int{"0": 0, "0.": 0, "0.001": 1, ".5": 500, "1": 1000, "1.": 1000, "1.000": 1000} {
		if got, valid := encodingQuality(value); !valid || got != want {
			t.Errorf("quality %q = %d, %v; want %d", value, got, valid, want)
		}
	}
}

func FuzzAcceptEncoding(f *testing.F) {
	for _, seed := range []string{"", "gzip,br", "gzip;q=0,identity;q=0", "*;q=0", "br;q=.5", "gzip;q=.", "GZIP;Q=1", "gzip;q=NaN"} {
		f.Add(seed, "gzip;q=0")
	}
	f.Fuzz(func(t *testing.T, first, second string) {
		left, leftOK := parseAcceptEncoding(http.Header{"Accept-Encoding": {first, second}})
		right, rightOK := parseAcceptEncoding(http.Header{"Accept-Encoding": {second, first}})
		if leftOK != rightOK {
			t.Fatal("field order changed syntax validity")
		}
		if !leftOK {
			return
		}
		if !maps.Equal(left.quality, right.quality) {
			t.Fatal("field order changed negotiated preferences")
		}
		for _, quality := range left.quality {
			if quality < 0 || quality > 1000 {
				t.Fatalf("invalid quality %d", quality)
			}
		}
	})
}

func TestCompressionNegotiationCannotTransform(t *testing.T) {
	for _, tc := range []struct {
		name              string
		request, response http.Header
		status            int
	}{
		{name: "request no-transform", request: http.Header{"Cache-Control": {"no-transform"}}},
		{name: "response no-transform", response: http.Header{"Cache-Control": {"no-transform"}}},
		{name: "partial status", status: 206},
		{name: "partial header", response: http.Header{"Content-Range": {"bytes 0-3/5"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, forbidden := range []bool{false, true} {
				base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					maps.Copy(w.Header(), tc.response)
					if tc.status != 0 {
						w.WriteHeader(tc.status)
					}
					_, _ = io.WriteString(w, "body")
				})
				assertCompressionTransformPolicy(t, base, tc.request, forbidden)
			}
		})
	}
}

func assertCompressionTransformPolicy(t *testing.T, base http.Handler, headers http.Header, forbidden bool) {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	maps.Copy(req.Header, headers)
	req.Header.Set("Accept-Encoding", "gzip")
	if forbidden {
		req.Header.Set("Accept-Encoding", "gzip, identity;q=0")
	}
	res := runRequest(t, chain(t, base, Compress(Gzip)), req)
	if forbidden && (res.Code != 406 || res.Body.Len() != 0) {
		t.Fatal("delivered forbidden identity")
	}
	if !forbidden && (res.Body.String() != "body" || res.Header().Get("Content-Encoding") != "") {
		t.Fatal("transformed a protected representation")
	}
}

func TestCompressionNegotiationPreservesBodylessAndErrors(t *testing.T) {
	for _, status := range []int{204, 205, 304, 401, 403, 404, 502, 503} {
		base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(status)
			if status >= 400 {
				_, _ = io.WriteString(w, "error body")
			}
		})
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Accept-Encoding", "zstd,identity;q=0")
		res := runRequest(t, chain(t, base, Compress(Gzip)), req)
		if res.Code != status || res.Body.Len() != 0 || res.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("lost bodyless/error semantics: %d => %d, %v %q", status, res.Code, res.Header(), res.Body.String())
		}
	}
}

func TestCompressionRejectionStripsMetadataAndTrailers(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, name := range []string{"Content-Type", "Content-Encoding", "Content-Digest", "ETag", "Last-Modified", "Content-Range", "Accept-Ranges"} {
			w.Header().Set(name, "origin")
		}
		w.Header().Set("Trailer", "Content-Length, Content-Digest, X-Finished")
		w.Header().Set("X-Policy", "retain")
		_, _ = io.WriteString(w, "unacceptable")
		w.Header().Set("Content-Length", "999")
		w.Header().Set("Content-Digest", "late")
		w.Header().Set("X-Finished", "late")
		w.Header().Set(http.TrailerPrefix+"ETag", "late")
	})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := runRequest(t, chain(t, base, ETag(), Cache("1h"), Compress(Gzip)), req)
	if res.Code != 406 || res.Body.Len() != 0 || res.Header().Get("Content-Length") != "0" || res.Header().Get("X-Policy") != "retain" {
		t.Fatalf("invalid rejection: %d %v %q", res.Code, res.Header(), res.Body.String())
	}
	for _, name := range []string{"Content-Type", "Content-Encoding", "Content-Digest", "ETag", "Last-Modified", "Content-Range", "Accept-Ranges", "Trailer", "X-Finished", http.TrailerPrefix + "ETag"} {
		if _, present := cacheHeaderValues(res.Header(), name); present {
			t.Fatalf("retained rejected metadata %s", name)
		}
	}
}
