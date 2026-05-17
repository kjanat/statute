package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newCFStub returns an httptest.Server that mimics the small subset of the
// Cloudflare API used by AddTXTRecord / DeleteRecord / FindZoneID. It tracks
// every request for assertions.
func newCFStub(t *testing.T) (*httptest.Server, *cfStub) {
	t.Helper()
	st := &cfStub{
		zones: map[string]string{
			"example.com": "zone-example",
			"sub.test":    "zone-sub-test",
		},
		records: map[string]map[string]string{},
		expect:  "Bearer test-token",
	}
	srv := httptest.NewServer(http.HandlerFunc(st.handle))
	t.Cleanup(srv.Close)
	return srv, st
}

type cfStub struct {
	zones   map[string]string            // name -> id
	records map[string]map[string]string // zoneID -> recordID -> value
	expect  string                       // expected Authorization header
	calls   int
}

func (s *cfStub) handle(w http.ResponseWriter, r *http.Request) {
	s.calls++
	if r.Header.Get("Authorization") != s.expect {
		s.write(w, false, nil, &cfError{Code: 9103, Message: "unauthorized"})
		return
	}
	isCreate := r.Method == http.MethodPost &&
		strings.HasPrefix(r.URL.Path, "/zones/") &&
		strings.HasSuffix(r.URL.Path, "/dns_records")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/zones":
		s.handleListZones(w, r)
	case isCreate:
		s.handleCreateRecord(w, r)
	case r.Method == http.MethodDelete:
		s.handleDeleteRecord(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *cfStub) handleListZones(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if id, ok := s.zones[name]; ok {
		type zone struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		s.write(w, true, []zone{{ID: id, Name: name}}, nil)
		return
	}
	s.write(w, true, []any{}, nil)
}

func (s *cfStub) handleCreateRecord(w http.ResponseWriter, r *http.Request) {
	zone := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/zones/"), "/dns_records")
	var body struct {
		Type, Name, Content string
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.write(w, false, nil, &cfError{Code: 9020, Message: "invalid JSON body: " + err.Error()})
		return
	}
	if s.records[zone] == nil {
		s.records[zone] = map[string]string{}
	}
	recID := "rec-" + body.Name
	s.records[zone][recID] = body.Content
	s.write(w, true, map[string]string{"id": recID}, nil)
}

func (s *cfStub) handleDeleteRecord(w http.ResponseWriter, r *http.Request) {
	// /zones/{zone}/dns_records/{rec}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 4 && parts[0] == "zones" && parts[2] == "dns_records" {
		delete(s.records[parts[1]], parts[3])
		s.write(w, true, map[string]string{"id": parts[3]}, nil)
		return
	}
	http.NotFound(w, r)
}

func (s *cfStub) write(w http.ResponseWriter, success bool, result any, e *cfError) {
	resp := struct {
		Success bool      `json:"success"`
		Errors  []cfError `json:"errors"`
		Result  any       `json:"result"`
	}{Success: success, Result: result}
	if e != nil {
		resp.Errors = []cfError{*e}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func TestCloudflareAPI_AddDeleteRecord(t *testing.T) {
	t.Parallel()
	srv, st := newCFStub(t)
	c := New("test-token")
	c.base = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	id, err := c.AddTXTRecord(ctx, "zone-example", "_acme-challenge.example.com", "abc123")
	if err != nil {
		t.Fatalf("AddTXTRecord: %v", err)
	}
	if id == "" {
		t.Fatal("AddTXTRecord returned empty id")
	}
	if st.records["zone-example"][id] != "abc123" {
		t.Errorf("record content: got %q", st.records["zone-example"][id])
	}

	if err := c.DeleteRecord(ctx, "zone-example", id); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if _, exists := st.records["zone-example"][id]; exists {
		t.Errorf("record still present after delete")
	}
}

func TestCloudflareAPI_FindZoneIDWalk(t *testing.T) {
	t.Parallel()
	srv, _ := newCFStub(t)
	c := New("test-token")
	c.base = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// FindZoneID should walk labels: foo.example.com → example.com (match)
	id, err := c.FindZoneID(ctx, "foo.example.com")
	if err != nil {
		t.Fatalf("FindZoneID: %v", err)
	}
	if id != "zone-example" {
		t.Errorf("zone: got %q", id)
	}

	// Wildcard-stripped lookup: *.example.com → example.com
	id, err = c.FindZoneID(ctx, "*.example.com")
	if err != nil {
		t.Fatalf("wildcard FindZoneID: %v", err)
	}
	if id != "zone-example" {
		t.Errorf("wildcard zone: got %q", id)
	}

	// No matching zone.
	if _, err := c.FindZoneID(ctx, "nowhere.invalid"); err == nil {
		t.Errorf("want error for missing zone")
	}
}

func TestCloudflareAPI_AuthFailure(t *testing.T) {
	t.Parallel()
	srv, _ := newCFStub(t)
	c := New("wrong-token")
	c.base = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.AddTXTRecord(ctx, "zone-example", "name", "val")
	if err == nil {
		t.Fatal("want error for bad token")
	}
}

func TestCloudflareAPI_ContextCancelled(t *testing.T) {
	t.Parallel()
	srv, _ := newCFStub(t)
	c := New("test-token")
	c.base = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call

	if _, err := c.AddTXTRecord(ctx, "zone-example", "name", "val"); err == nil {
		t.Fatal("want error from cancelled context")
	}
}

func TestCloudflareAPI_NetworkError(t *testing.T) {
	t.Parallel()
	c := New("test-token")
	c.base = "http://127.0.0.1:1" // nothing listens here; connection refused

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.FindZoneID(ctx, "foo.example.com"); err == nil {
		t.Fatal("want error when the API is unreachable")
	}
}
