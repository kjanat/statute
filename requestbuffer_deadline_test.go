package statute

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestBufferDeadlineRetiresWithIngress(t *testing.T) {
	var calls atomic.Int32
	state := &requestBufferRequest{deadline: func(time.Time) error { calls.Add(1); return nil }}
	if !state.beginRead() {
		t.Fatal("live ingress rejected read")
	}
	state.finish()
	if calls.Load() != 1 {
		t.Fatal("unfinished read was not interrupted before retirement")
	}
	state.interruptRead()
	if state.beginRead() || calls.Load() != 1 {
		t.Fatal("retired ingress allowed response writer access")
	}
	state.endRead()
	state.finish()
	if calls.Load() != 1 {
		t.Fatal("duplicate retirement touched writer")
	}
}

func TestRequestBufferIngressWaitsForDeadlineCall(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	state := &requestBufferRequest{deadline: func(time.Time) error { close(entered); <-release; return nil }}
	callbackDone := make(chan struct{})
	go func() { defer close(callbackDone); state.interruptRead() }()
	<-entered
	started := make(chan struct{})
	go func() { close(started); state.finish(); close(finished) }()
	<-started
	select {
	case <-finished:
		t.Error("ingress retired with a deadline call still active")
	default:
	}
	close(release)
	<-callbackDone
	<-finished
	// A late callback must not invoke the retired writer again.
	state.interruptRead()
}

func TestRequestBufferLateProducerCannotReadIngress(t *testing.T) {
	m := bufferTestManager(t, false, 6, 0)
	state := &requestBufferRequest{manager: m, deadline: func(time.Time) error { t.Error("retired writer accessed"); return nil }}
	state.finish()
	body := &requestBufferFaultBody{read: func([]byte) (int, error) { t.Error("late producer read ingress body"); return 0, io.EOF }}
	req := httptest.NewRequest("POST", "/", body)
	req = req.WithContext(context.WithValue(req.Context(), requestBufferContextKey{}, state))
	policy, err := resolveMiddleware(RequestBuffer("5B"))
	if err != nil {
		t.Fatal(err)
	}
	h := requestBufferHandler(policy, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("late producer invoked downstream") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 || body.reads != 0 {
		t.Fatalf("status=%d reads=%d", rec.Code, body.reads)
	}
	assertBufferReleased(t, m)
}
