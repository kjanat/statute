package statute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

type passiveAttemptKey struct{}

type passiveAttempt struct{ once sync.Once }

func recordPassiveAttempt(r *http.Request, record func(*http.Request)) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	if attempt, _ := r.Context().Value(passiveAttemptKey{}).(*passiveAttempt); attempt != nil {
		attempt.once.Do(func() { record(r) })
	} else {
		record(r)
	}
}

type passiveBodyTransport struct {
	base   http.RoundTripper
	record func(*http.Request)
}

func (t *passiveBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil && resp.Body != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body = &passiveResponseBody{ReadCloser: resp.Body, request: r, record: t.record}
	}
	return resp, err
}

// Only raw upstream reads supply evidence; transformed or downstream errors do not.
type passiveResponseBody struct {
	io.ReadCloser
	request *http.Request
	record  func(*http.Request)
}

func (b *passiveResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		recordPassiveAttempt(b.request, b.record)
	}
	return n, err
}
