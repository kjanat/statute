package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamEventsConnectionBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"event", http.StatusOK, `{"Type":"container","Action":"start"}`, "connected,event"},
		{"empty", http.StatusOK, "", "connected"},
		{"malformed", http.StatusOK, "not json", "connected"},
		{"rejected", http.StatusServiceUnavailable, "unavailable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(daemon.Close)
			client, err := NewClient("tcp://" + strings.TrimPrefix(daemon.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			var order []string
			err = client.StreamEvents(context.Background(), func() {
				order = append(order, "connected")
			}, func(Event) { order = append(order, "event") })
			if err == nil {
				t.Fatal("closed stream returned no error")
			}
			if got := strings.Join(order, ","); got != tc.want {
				t.Fatalf("callbacks = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStreamEventsCancelledSubscriptionDoesNotConnect(t *testing.T) {
	client := fakeDaemon(t, "[]", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.StreamEvents(ctx, func() {
		t.Error("cancelled subscription reported connected")
	}, func(Event) { t.Error("cancelled subscription delivered an event") }); err == nil {
		t.Fatal("cancelled subscription returned no error")
	}
}
