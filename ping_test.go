package statute

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPingUsesCurrentProcessReadiness(t *testing.T) {
	for _, tc := range []struct {
		handler     http.Handler
		terminating int
	}{{Ping(), 503}, {Ping(http.StatusAccepted), 202}} {
		var ready atomic.Bool
		wrapped := processState(&ready, tc.handler)
		for _, serving := range []bool{true, false, true} {
			ready.Store(serving)
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
			want := http.StatusOK
			if !serving {
				want = tc.terminating
			}
			if w.Code != want || w.Body.String() != http.StatusText(want) {
				t.Fatalf("serving=%t status=%d body=%q", serving, w.Code, w.Body.String())
			}
		}
	}
}

func TestPingRejectsInvalidTerminatingStatus(t *testing.T) {
	_, err := Resolve(Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{Match("/ping").Handle(Ping(100))}})
	if err == nil {
		t.Fatal("invalid ping terminating status accepted")
	}
}
