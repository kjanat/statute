package htmlrewrite

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPStatuteCompressionPreservesBypass(t *testing.T) {
	const input = `<a class="rewrite">page</a>`
	var encoded bytes.Buffer
	zw := gzip.NewWriter(&encoded)
	_, _ = io.WriteString(zw, input)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("ETag", `"origin"`)
		if r.Header.Get("X-Test-Encoded") != "" {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(encoded.Bytes())
			return
		}
		w.Header().Set("Cache-Control", "no-transform")
		_, _ = io.WriteString(w, input)
	}))
	defer origin.Close()
	endpoint := startHTTPStatute(t, origin.URL)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, coding := range []bool{false, true} {
		req := httpTestRequest(t, "GET", endpoint+"/open-compressed")
		req.Header.Set("Accept-Encoding", "gzip")
		if coding {
			req.Header.Set("X-Test-Encoded", "yes")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		wantBody, wantCoding := []byte(input), ""
		if coding {
			wantBody, wantCoding = encoded.Bytes(), "gzip"
		}
		if res.StatusCode != 200 || !bytes.Equal(body, wantBody) || res.Header.Get("Content-Encoding") != wantCoding || res.Header.Get("ETag") != `"origin"` {
			t.Fatalf("changed bypass representation: %d %v %x", res.StatusCode, res.Header, body)
		}
	}
}

func TestHTTPStatuteCompressedBypassTrailers(t *testing.T) {
	const input = `<a class="rewrite">page</a>`
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Trailer", "Content-Digest, X-Finished")
		_, _ = io.WriteString(w, input)
		w.(http.Flusher).Flush()
		w.Header().Set("Content-Digest", "origin-identity")
		w.Header().Set("X-Finished", "yes")
	}))
	defer origin.Close()
	endpoint := startHTTPStatute(t, origin.URL)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	req := httpTestRequest(t, "GET", endpoint+"/open-compressed")
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("compressed bypass: %d %v", res.StatusCode, res.Header)
	}
	gz, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	body, err := io.ReadAll(gz)
	if err != nil || string(body) != input {
		t.Fatalf("bypass body: %q %v", body, err)
	}
	if res.Trailer.Get("Content-Digest") != "" || res.Trailer.Get("X-Finished") != "yes" {
		t.Fatalf("compressed trailers: %v", res.Trailer)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("bypass lost no-store: %v", res.Header)
	}
}
