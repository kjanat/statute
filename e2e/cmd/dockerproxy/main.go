//go:build e2e

// The dockerproxy actor forwards the real Docker API and records every start
// request, including idempotent duplicates that produce no Docker start event.
package main

import (
	"context"
	"encoding/json"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

func main() {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
		},
	}
	srv := &http.Server{
		Addr: ":2375", Handler: newRecorder(transport), ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

type startSnapshot struct {
	Total  uint64            `json:"total"`
	Starts map[string]uint64 `json:"starts"`
}

type recorder struct {
	proxy  *httputil.ReverseProxy
	mu     sync.Mutex
	starts map[string]uint64
	total  uint64
}

func newRecorder(transport http.RoundTripper) *recorder {
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "docker"})
	proxy.Transport = transport
	proxy.FlushInterval = -1 // Docker events must arrive before the stream closes.
	return &recorder{proxy: proxy, starts: make(map[string]uint64)}
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && req.URL.Path == "/debug/starts" {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(r.snapshot()); err != nil {
			log.Printf("dockerproxy: write start counters: %v", err)
		}
		return
	}
	if ref := startReference(req); ref != "" {
		r.mu.Lock()
		r.starts[ref]++
		r.total++
		r.mu.Unlock()
	}
	r.proxy.ServeHTTP(w, req)
}

func (r *recorder) snapshot() startSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return startSnapshot{Total: r.total, Starts: maps.Clone(r.starts)}
}

// startReference accepts unversioned and versioned API paths without changing
// the forwarded request. Counts include failures and already-running replies.
func startReference(req *http.Request) string {
	if req.Method != http.MethodPost {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
	if len(parts) == 4 && strings.HasPrefix(parts[0], "v") {
		parts = parts[1:]
	}
	if len(parts) == 3 && parts[0] == "containers" && parts[1] != "" && parts[2] == "start" {
		return parts[1]
	}
	return ""
}
