//go:build statute_htmlrewrite && htmlrewrite_research

package htmlrewrite

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixture observes Docker API calls from a separate Statute process.
type rewriteDaemon struct {
	mu         sync.Mutex
	id, origin string
	hosts      string
	running    bool
	stops      map[string]int
}

func (d *rewriteDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/events" {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/_ping":
		_, _ = io.WriteString(w, "OK")
	case "/containers/json":
		d.list(w)
	case "/containers/" + d.id + "/json":
		_ = json.NewEncoder(w).Encode(map[string]any{"State": map[string]any{"Running": d.running}})
	case "/containers/" + d.id + "/start":
		d.running = true
		w.WriteHeader(204)
	case "/containers/" + d.id + "/stop":
		d.stops[d.id]++
		d.running = false
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

func (d *rewriteDaemon) list(w http.ResponseWriter) {
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(d.origin, "http://"))
	number, _ := strconv.Atoi(port)
	state := "exited"
	if d.running {
		state = "running"
	}
	_ = json.NewEncoder(w).Encode([]map[string]any{{
		"Id": d.id, "Names": []string{"/page"}, "State": state,
		"Labels": map[string]string{
			"statute.enable": "true", "statute.service": "page", "statute.path": "/*",
			"statute.host": d.hosts, "statute.port": port,
		},
		"Ports":           []map[string]any{{"PrivatePort": number, "Type": "tcp"}},
		"NetworkSettings": map[string]any{"Networks": map[string]any{"test": map[string]string{"IPAddress": host}}},
	}})
}

func (d *rewriteDaemon) stopCount(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stops[id]
}

func (d *rewriteDaemon) replace(id, origin string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.id, d.origin, d.running = id, origin, true
}

func startDockerRewriteStatute(t *testing.T, origin string) (string, *rewriteDaemon) {
	t.Helper()
	d := &rewriteDaemon{id: "old-container", origin: origin, hosts: "rewrite.test,plain.test,open.test", running: true, stops: make(map[string]int)}
	daemon := httptest.NewServer(d)
	t.Cleanup(daemon.Close)
	endpoint, _ := startHTTPStatuteConfigured(t, origin, "", []string{
		"STATUTE_HTML_DOCKER_ENDPOINT=" + daemon.URL,
		"STATUTE_HTML_DOCKER_STORAGE=" + t.TempDir(),
	})
	return endpoint, d
}

func dockerRewriteGet(t *testing.T, client *http.Client, endpoint, host string) *http.Response {
	t.Helper()
	req := httpTestRequest(t, "GET", endpoint)
	req.Host = host
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestHTTPDockerRewriteLifetime(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint("replace=", replace), func(t *testing.T) { testDockerRewriteLifetime(t, replace) })
	}
}

func testDockerRewriteLifetime(t *testing.T, replace bool) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	releaseOrigin := func() { once.Do(func() { close(release) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<a class="rewrite">old</a>`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(origin.Close)
	t.Cleanup(releaseOrigin)
	endpoint, d := startDockerRewriteStatute(t, origin.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	res := dockerRewriteGet(t, client, endpoint, "rewrite.test")
	defer res.Body.Close()
	prefix := make([]byte, len(`<a class="rewrite" href="https://example.invalid/rewritten">old<em>inserted</em>`))
	if _, err := io.ReadFull(res.Body, prefix); err != nil || !strings.Contains(string(prefix), "inserted") {
		t.Fatalf("no rewritten Docker stream: %d %q %v", res.StatusCode, prefix, err)
	}
	baseline := d.stopCount("old-container")
	if replace {
		successor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<a class="rewrite">new</a>`)
		}))
		t.Cleanup(successor.Close)
		d.replace("new-container", successor.URL)
		waitDockerStop(t, d, "new-container", 0)
	} else {
		timer := time.NewTimer(600 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		if d.stopCount("old-container") != baseline {
			t.Fatal("rewritten stream lost workload ownership")
		}
	}
	releaseOrigin()
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if !replace {
		waitDockerStop(t, d, "old-container", baseline)
		return
	}
	if d.stopCount("old-container") != baseline {
		t.Fatal("retired completion issued an old-container stop")
	}
	for _, host := range []string{"plain.test", "rewrite.test", "open.test"} {
		res := dockerRewriteGet(t, client, endpoint, host)
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != 200 || !strings.Contains(string(body), "new") || strings.Contains(string(body), "inserted") != (host != "plain.test") {
			t.Fatalf("successor policy leaked on %s: %d %q %v", host, res.StatusCode, body, err)
		}
	}
	waitDockerStop(t, d, "new-container", 1)
}

func waitDockerStop(t *testing.T, d *rewriteDaemon, id string, baseline int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for d.stopCount(id) == baseline {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("idle stop missing for %s", id)
		}
	}
}

func TestHTTPDockerRouterPolicyChange(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseOrigin := func() { once.Do(func() { close(release) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<a class="rewrite">prefix</a>`)
		if r.URL.Path != "/hold" {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `<a class="rewrite">tail</a>`)
	}))
	t.Cleanup(origin.Close)
	t.Cleanup(releaseOrigin)
	endpoint, d := startDockerRewriteStatute(t, origin.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	res := dockerRewriteGet(t, client, endpoint+"/hold", "rewrite.test")
	defer res.Body.Close()
	prefix := make([]byte, len(`<a class="rewrite" href="https://example.invalid/rewritten">prefix<em>inserted</em>`))
	if _, err := io.ReadFull(res.Body, prefix); err != nil || !strings.Contains(string(prefix), "inserted") {
		t.Fatalf("prefix: %q %v", prefix, err)
	}
	baseline := d.stopCount("old-container")
	d.mu.Lock()
	d.hosts = "plain.test"
	d.mu.Unlock()
	waitDockerRouteStatus(t, client, endpoint+"/fresh", "rewrite.test", 404)
	plain := dockerRewriteGet(t, client, endpoint+"/fresh", "plain.test")
	body, err := io.ReadAll(plain.Body)
	_ = plain.Body.Close()
	if err != nil || plain.StatusCode != 200 || strings.Contains(string(body), "inserted") {
		t.Fatalf("new route policy: %d %q %v", plain.StatusCode, body, err)
	}
	time.Sleep(600 * time.Millisecond)
	if d.stopCount("old-container") != baseline {
		t.Fatal("routing revision dropped the outstanding stream lease")
	}
	releaseOrigin()
	tail, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || strings.Count(string(tail), "<em>inserted</em>") != 1 {
		t.Fatalf("old stream lost its captured rewrite policy: %q %v", tail, err)
	}
	waitDockerStop(t, d, "old-container", baseline)
}

func waitDockerRouteStatus(t *testing.T, client *http.Client, endpoint, host string, status int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := dockerRewriteGet(t, client, endpoint, host)
		_, err := io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("route %s remained %d, want %d", host, res.StatusCode, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
