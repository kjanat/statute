package statute

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
)

type (
	processReadyKey struct{}
	processPing     struct{ terminatingStatus int }
)

// Ping answers 200 while serving and terminatingStatus after shutdown starts.
// The default terminating status is 503. The body is the HTTP status text.
func Ping(terminatingStatus ...int) http.Handler {
	status := http.StatusServiceUnavailable
	if len(terminatingStatus) > 0 {
		status = terminatingStatus[0]
	}
	return &processPing{terminatingStatus: status}
}

func (p *processPing) validateHandler() error {
	if p.terminatingStatus < 200 || p.terminatingStatus > 599 {
		return errors.New("ping: invalid terminating status")
	}
	return nil
}

func (p *processPing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	if ready, _ := r.Context().Value(processReadyKey{}).(*atomic.Bool); ready != nil && !ready.Load() {
		status = p.terminatingStatus
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(http.StatusText(status)))
}

func processState(ready *atomic.Bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), processReadyKey{}, ready)))
	})
}
