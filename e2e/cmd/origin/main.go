//go:build e2e

// The origin actor is a real upstream process for the e2e lane: a
// behavior-rich HTTP server the compiled Statute binary proxies to over
// real sockets. One binary serves as origin-1 or origin-2 depending on
// ORIGIN_ID; TLS material is optional and turns the listener into an
// HTTPS (and HTTP/2) origin for upstream-TLS scenarios.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	id := envDefault("ORIGIN_ID", "origin")
	addr := envDefault("ORIGIN_ADDR", ":7000")
	o := newOrigin(id)
	srv := &http.Server{
		Addr:              addr,
		Handler:           o,
		ReadHeaderTimeout: 10 * time.Second,
	}
	cert, key := os.Getenv("ORIGIN_TLS_CERT"), os.Getenv("ORIGIN_TLS_KEY")
	log.SetFlags(0)
	log.Printf(`{"origin":%q,"event":"listening","addr":%q,"tls":%v}`, id, addr, cert != "")
	var err error
	if cert != "" {
		err = srv.ListenAndServeTLS(cert, key)
	} else {
		err = srv.ListenAndServe()
	}
	log.Fatalf(`{"origin":%q,"event":"exit","err":%q}`, id, err)
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// origin holds the mutable behavior state one instance exposes through
// its /admin endpoints: the health toggle, per-key failure budgets, and
// the request journal edge assertions read back.
type origin struct {
	id  string
	mux *http.ServeMux

	mu      sync.Mutex
	healthy bool
	// failLeft counts remaining forced failures per key for /fail.
	failLeft map[string]int
	journal  []journalEntry
}

// journalEntry records one served request exactly as the origin saw it —
// the harness proves rewrite and header semantics from this view, so the
// path must be the as-received one, never a normalized form.
type journalEntry struct {
	Origin    string    `json:"origin"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Query     string    `json:"query,omitempty"`
	Host      string    `json:"host"`
	Proto     string    `json:"proto"`
	RequestID string    `json:"request_id,omitempty"`
	Forwarded string    `json:"x_forwarded_for,omitempty"`
	SNI       string    `json:"sni,omitempty"`
	Time      time.Time `json:"time"`
}

func newOrigin(id string) *origin {
	o := &origin{id: id, healthy: true, failLeft: make(map[string]int)}
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", o.handleEcho)
	mux.HandleFunc("/health", o.handleHealth)
	mux.HandleFunc("/slow", o.handleSlow)
	mux.HandleFunc("/stream", o.handleStream)
	mux.HandleFunc("/upgrade", o.handleUpgrade)
	mux.HandleFunc("/fail", o.handleFail)
	mux.HandleFunc("/admin/health", o.handleAdminHealth)
	mux.HandleFunc("/admin/requests", o.handleAdminRequests)
	mux.HandleFunc("/", o.handleEcho)
	o.mux = mux
	return o
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/admin/") {
		e := o.record(r)
		if b, err := json.Marshal(e); err == nil {
			log.Println(string(b))
		}
	}
	o.mux.ServeHTTP(w, r)
}

func (o *origin) record(r *http.Request) journalEntry {
	e := journalEntry{
		Origin:    o.id,
		Method:    r.Method,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
		Host:      r.Host,
		Proto:     r.Proto,
		RequestID: r.Header.Get("X-Request-Id"),
		Forwarded: r.Header.Get("X-Forwarded-For"),
		Time:      time.Now(),
	}
	if r.TLS != nil {
		e.SNI = r.TLS.ServerName
	}
	o.mu.Lock()
	o.journal = append(o.journal, e)
	o.mu.Unlock()
	return e
}

// echoBody is the response shape every proxied assertion parses; the
// "origin" field is how a result names the backend that served it.
type echoBody struct {
	Origin    string `json:"origin"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Query     string `json:"query,omitempty"`
	Host      string `json:"host"`
	Proto     string `json:"proto"`
	RequestID string `json:"request_id,omitempty"`
	Forwarded string `json:"x_forwarded_for,omitempty"`
	SNI       string `json:"sni,omitempty"`
}

func (o *origin) handleEcho(w http.ResponseWriter, r *http.Request) {
	body := echoBody{
		Origin:    o.id,
		Method:    r.Method,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
		Host:      r.Host,
		Proto:     r.Proto,
		RequestID: r.Header.Get("X-Request-Id"),
		Forwarded: r.Header.Get("X-Forwarded-For"),
	}
	if r.TLS != nil {
		body.SNI = r.TLS.ServerName
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf(`{"origin":%q,"event":"encode_error","err":%q}`, o.id, err.Error())
	}
}

func (o *origin) handleHealth(w http.ResponseWriter, _ *http.Request) {
	o.mu.Lock()
	healthy := o.healthy
	o.mu.Unlock()
	if !healthy {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

func (o *origin) handleSlow(w http.ResponseWriter, r *http.Request) {
	d, err := time.ParseDuration(r.URL.Query().Get("d"))
	if err != nil {
		http.Error(w, "bad duration", http.StatusBadRequest)
		return
	}
	time.Sleep(d)
	o.handleEcho(w, r)
}

func (o *origin) handleStream(w http.ResponseWriter, r *http.Request) {
	chunks := queryInt(r, "chunks", 5)
	interval := 100 * time.Millisecond
	if v := r.URL.Query().Get("interval"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	for i := range chunks {
		fmt.Fprintf(w, "chunk %d from %s\n", i, o.id)
		f.Flush()
		if i < chunks-1 {
			time.Sleep(interval)
		}
	}
}

// handleUpgrade hijacks the connection into a one-line echo protocol.
// It proves connection-upgrade passthrough without a WebSocket
// dependency: the proxy must switch protocols and then carry raw bytes
// both ways.
func (o *origin) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "echo") {
		http.Error(w, "upgrade: echo required", http.StatusUpgradeRequired)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijacker", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
	rw.Flush()
	echoLines(conn, rw, o.id)
}

// echoLines answers each received line with "<origin>: <line>" until the
// peer closes.
func echoLines(conn net.Conn, rw *bufio.ReadWriter, id string) {
	for {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		fmt.Fprintf(rw, "%s: %s", id, line)
		if rw.Flush() != nil {
			return
		}
	}
}

// handleFail returns 502 for the first n requests per key, then echoes.
// The budget arms on first sight of a key, so a retry scenario forces
// exactly n backend failures without pre-seeding origin state. An
// origin query parameter scopes the failures to one instance, giving
// scenarios a deterministic backend asymmetry behind a shared pool.
func (o *origin) handleFail(w http.ResponseWriter, r *http.Request) {
	if only := r.URL.Query().Get("origin"); only != "" && only != o.id {
		o.handleEcho(w, r)
		return
	}
	key := r.URL.Query().Get("key")
	n := queryInt(r, "n", 1)
	o.mu.Lock()
	left, seen := o.failLeft[key]
	if !seen {
		left = n
	}
	fail := left > 0
	if fail {
		left--
	}
	o.failLeft[key] = left
	o.mu.Unlock()
	if fail {
		http.Error(w, "forced failure", http.StatusBadGateway)
		return
	}
	o.handleEcho(w, r)
}

func (o *origin) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state != "up" && state != "down" {
		http.Error(w, "state must be up or down", http.StatusBadRequest)
		return
	}
	o.mu.Lock()
	o.healthy = state == "up"
	o.mu.Unlock()
	fmt.Fprintln(w, state)
}

func (o *origin) handleAdminRequests(w http.ResponseWriter, _ *http.Request) {
	o.mu.Lock()
	entries := append([]journalEntry(nil), o.journal...)
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		log.Printf(`{"origin":%q,"event":"encode_error","err":%q}`, o.id, err.Error())
	}
}

func queryInt(r *http.Request, key string, fallback int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}
