package docker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeDaemon serves the two Docker Engine API endpoints the client uses.
func fakeDaemon(t *testing.T, containers string, events []string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(containers))
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for _, ev := range events {
			_, _ = w.Write([]byte(ev + "\n"))
			fl.Flush()
		}
		<-r.Context().Done()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client, err := NewClient("tcp://" + strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

const containersFixture = `[
  {
    "Id": "abc123",
    "Names": ["/web-1"],
    "State": "running",
    "Labels": {"statute.enable": "true"},
    "Ports": [
      {"PrivatePort": 8080, "PublicPort": 32768, "Type": "tcp"},
      {"PrivatePort": 8080, "Type": "tcp"},
      {"PrivatePort": 5353, "Type": "udp"}
    ],
    "NetworkSettings": {"Networks": {
      "bridge": {"IPAddress": "172.17.0.2"},
      "none": {"IPAddress": ""}
    }}
  }
]`

func TestListContainers(t *testing.T) {
	client := fakeDaemon(t, containersFixture, nil)
	got, err := client.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	want := []Container{{
		ID:       "abc123",
		Name:     "web-1",
		Labels:   map[string]string{"statute.enable": "true"},
		Networks: map[string]string{"bridge": "172.17.0.2"},
		Ports:    []int{8080},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListContainers = %+v, want %+v", got, want)
	}
}

func TestPing(t *testing.T) {
	client := fakeDaemon(t, "[]", nil)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestStreamEvents(t *testing.T) {
	events := []string{
		`{"Type":"container","Action":"start"}`,
		`{"Type":"container","Action":"exec_create: sh"}`,
		`{"Type":"container","Action":"die"}`,
	}
	client := fakeDaemon(t, "[]", events)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got []Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = client.StreamEvents(ctx, func(ev Event) {
			got = append(got, ev)
			if len(got) == len(events) {
				cancel()
			}
		})
	}()
	<-done

	if len(got) != 3 {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	if !got[0].ChangesTopology() || got[1].ChangesTopology() || !got[2].ChangesTopology() {
		t.Errorf("topology classification wrong: %+v", got)
	}
}

func TestEventChangesTopology(t *testing.T) {
	tests := []struct {
		ev   Event
		want bool
	}{
		{Event{Type: "container", Action: "start"}, true},
		{Event{Type: "container", Action: "die"}, true},
		{Event{Type: "container", Action: "health_status: healthy"}, true},
		{Event{Type: "container", Action: "exec_start: ls"}, false},
		{Event{Type: "network", Action: "connect"}, false},
	}
	for _, tt := range tests {
		if got := tt.ev.ChangesTopology(); got != tt.want {
			t.Errorf("%+v.ChangesTopology() = %v, want %v", tt.ev, got, tt.want)
		}
	}
}

func TestNewClientEndpoints(t *testing.T) {
	if _, err := NewClient("unix:///var/run/docker.sock"); err != nil {
		t.Errorf("unix endpoint: %v", err)
	}
	if _, err := NewClient("tcp://127.0.0.1:2375"); err != nil {
		t.Errorf("tcp endpoint: %v", err)
	}
	if _, err := NewClient("ssh://host"); err == nil {
		t.Errorf("ssh endpoint accepted")
	}
}

// TestNormalizeContainerNoName covers daemons that omit Names.
func TestNormalizeContainerNoName(t *testing.T) {
	var cj containerJSON
	if err := json.Unmarshal([]byte(`{"Id":"deadbeef"}`), &cj); err != nil {
		t.Fatal(err)
	}
	c := normalizeContainer(cj)
	if c.Name != "deadbeef" {
		t.Errorf("Name = %q", c.Name)
	}
}

// brokenDaemon serves the given status and body on every endpoint.
func brokenDaemon(t *testing.T, status int, body string) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	client, err := NewClient("tcp://" + strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestClientStatusErrors(t *testing.T) {
	// Non-200 responses surface as "unexpected status".
	client := brokenDaemon(t, http.StatusInternalServerError, "boom")
	if _, err := client.ListContainers(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected status") {
		t.Errorf("ListContainers 500: %v", err)
	}
	if err := client.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected status") {
		t.Errorf("Ping 500: %v", err)
	}
	if err := client.StreamEvents(context.Background(), func(Event) {}); err == nil || !strings.Contains(err.Error(), "unexpected status") {
		t.Errorf("StreamEvents 500: %v", err)
	}
}

func TestClientDecodeErrors(t *testing.T) {
	// Malformed JSON surfaces as a decode error.
	client := brokenDaemon(t, http.StatusOK, "{not json")
	if _, err := client.ListContainers(context.Background()); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("ListContainers malformed body: %v", err)
	}
	if err := client.StreamEvents(context.Background(), func(Event) {}); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("StreamEvents malformed body: %v", err)
	}
}

// TestUnixSocketEndToEnd exercises NewClient's unix:// branch against a
// real unix-socket listener.
func TestUnixSocketEndToEnd(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("unix listen: %v", err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			_, _ = w.Write([]byte("OK"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)

	client, err := NewClient("unix://" + sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping over unix socket: %v", err)
	}
	got, err := client.ListContainers(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("ListContainers over unix socket: %v %v", got, err)
	}
}
