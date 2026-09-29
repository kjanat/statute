//go:build e2e

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func waitSpecForTest(t *testing.T, raw string) *waitPredicate {
	t.Helper()
	p, err := parseWaitSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWaitPredicates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec string
		body string
		want bool
	}{
		{"contains all", `{"contains":["ready","ok"]}`, "ready and ok", true},
		{"contains missing", `{"contains":["ready","ok"]}`, "ready", false},
		{"pointer escapes", `{"json":[{"pointer":"/a~1b/~0/0","equal":"ok"}]}`, `{"a/b":{"~":["ok"]}}`, true},
		{"null exists", `{"json":[{"pointer":"/state","equal":null}]}`, `{"state":null}`, true},
		{"null missing", `{"json":[{"pointer":"/state","equal":null}]}`, `{}`, false},
		{"array noncanonical index", `{"json":[{"pointer":"/01","equal":true}]}`, `[false,true]`, false},
		{"root scalar", `{"json":[{"pointer":"","equal":true}]}`, `true`, true},
		{"numeric exact", `{"json":[{"pointer":"/id","equal":18446744073709551615}]}`, `{"id":18446744073709551615}`, true},
		{"numeric no rounding", `{"json":[{"pointer":"/id","equal":18446744073709551615}]}`, `{"id":18446744073709551614}`, false},
		{"numeric equivalent", `{"json":[{"pointer":"/n","equal":1e2}]}`, `{"n":100.0}`, true},
		{"numeric minimum", `{"json":[{"pointer":"/n","min":2}]}`, `{"n":2}`, true},
		{"numeric strict", `{"json":[{"pointer":"/n","greater_than":0}]}`, `{"n":0.01}`, true},
		{"numeric strict zero", `{"json":[{"pointer":"/n","greater_than":0}]}`, `{"n":0}`, false},
		{"numeric type mismatch", `{"json":[{"pointer":"/n","min":2}]}`, `{"n":"3"}`, false},
		{"bounded exponent", `{"json":[{"pointer":"/n","min":2}]}`, `{"n":1e999999999}`, false},
		{"array length", `{"json":[{"pointer":"/owners","length":0}]}`, `{"owners":[]}`, true},
		{"object length", `{"json":[{"pointer":"/owners","length":1}]}`, `{"owners":{"a":1}}`, true},
		{"null has no length", `{"json":[{"pointer":"/owners","length":0}]}`, `{"owners":null}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := waitSpecForTest(t, tc.spec)
			got, err := p.matches(http.StatusOK, []byte(tc.body))
			if err != nil || got != tc.want {
				t.Fatalf("match = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestWaitInvalidSpecs(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`null`, `[]`, `{"typo":true}`, `{} {}`, `{"status":99}`, `{"consecutive":-1}`,
		`{"reject_contains":[""]}`, `{"json":[{"pointer":"abc","equal":0}]}`,
		`{"json":[{"pointer":"/~2","equal":0}]}`, `{"json":[{"pointer":"/~","equal":0}]}`,
		`{"json":[{"pointer":"/x"}]}`, `{"json":[{"pointer":"/x","equal":{},"min":0}]}`,
		`{"json":[{"pointer":"/x","equal":[]}]}`, `{"json":[{"pointer":"/x","length":-1}]}`,
		`{"json":[{"pointer":"/x","equal":0,"min":0}]}`, `{"json":[{"pointer":"/x","min":1e999999999}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseWaitSpec(raw); err == nil {
				t.Fatal("invalid spec accepted")
			}
		})
	}
}

func TestWaitFatalObservations(t *testing.T) {
	t.Parallel()
	p := waitSpecForTest(t, `{"reject_contains":["private-id"],"json":[{"pointer":"/ready","equal":true}]}`)
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusBadGateway, `{"private-id":1}`},
		{http.StatusOK, `broken`},
		{http.StatusOK, `{} {}`},
	} {
		_, err := p.matches(tc.status, []byte(tc.body))
		if err == nil {
			t.Fatalf("unsafe observation %d %q accepted", tc.status, tc.body)
		}
	}
	_, _, err := waitProgress(p, 200, []byte("private-id"), io.ErrUnexpectedEOF, 0)
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("partial response privacy failure was lost: %v", err)
	}
}

func TestWaitRetriesUnexpectedStatusBeforeJSON(t *testing.T) {
	t.Parallel()
	p := waitSpecForTest(t, `{"json":[{"pointer":"/ready","equal":true}]}`)
	calls, closed := 0, 0
	client := &http.Client{Transport: waitRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		status, body := http.StatusServiceUnavailable, "not ready yet"
		if calls > 1 {
			status, body = http.StatusOK, `{"ready":true}`
		}
		return &http.Response{StatusCode: status, Body: waitBody{strings.NewReader(body), &closed}}, nil
	})}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	body, err := pollWait(ctx, client, "http://example.test/diagnostics", p, time.Millisecond)
	if err != nil || string(body) != `{"ready":true}` || calls != 2 || closed != 2 {
		t.Fatalf("body %q, calls %d, closed %d, error %v", body, calls, closed, err)
	}
}

type waitRoundTripper func(*http.Request) (*http.Response, error)

func (f waitRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type waitBody struct {
	io.Reader
	closed *int
}

func (b waitBody) Close() error { *b.closed++; return nil }

func TestWaitConsecutiveObservations(t *testing.T) {
	t.Parallel()
	p := waitSpecForTest(t, `{"contains":["ready"],"consecutive":2}`)
	bodies := []string{"ready", "pending", "ready", "transport error", "ready", "ready"}
	calls, closed := 0, 0
	client := &http.Client{Transport: waitRoundTripper(func(*http.Request) (*http.Response, error) {
		body := bodies[calls]
		calls++
		if body == "transport error" {
			return nil, errors.New("connection reset")
		}
		return &http.Response{StatusCode: 200, Body: waitBody{strings.NewReader(body), &closed}}, nil
	})}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	body, err := pollWait(ctx, client, "http://example.test/diagnostics", p, time.Millisecond)
	if err != nil || string(body) != "ready" || calls != 6 || closed != 5 {
		t.Fatalf("body %q, calls %d, closed %d, error %v", body, calls, closed, err)
	}
}

type waitBlockedBody struct {
	ctx    context.Context
	closed *bool
}

func (b waitBlockedBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b waitBlockedBody) Close() error { *b.closed = true; return nil }

func TestWaitDeadlineCancelsBodyRead(t *testing.T) {
	t.Parallel()
	p := waitSpecForTest(t, "")
	closed := false
	client := &http.Client{Transport: waitRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: waitBlockedBody{r.Context(), &closed}}, nil
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := pollWait(ctx, client, "http://example.test/health", p, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) || !closed {
		t.Fatalf("body deadline: closed %v, error %v", closed, err)
	}
}

func TestWaitCanceledBeforeRequest(t *testing.T) {
	t.Parallel()
	p := waitSpecForTest(t, "")
	client := &http.Client{Transport: waitRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("request started after cancellation")
		return nil, context.Canceled
	})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := pollWait(ctx, client, "http://example.test/health", p, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestWaitPrivacyFailureIsImmediate(t *testing.T) {
	t.Parallel()
	p := waitSpecForTest(t, `{"reject_contains":["private-id"]}`)
	calls := 0
	client := &http.Client{Transport: waitRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("private-id"))}, nil
	})}
	_, err := pollWait(t.Context(), client, "http://example.test/diagnostics", p, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "forbidden") || calls != 1 {
		t.Fatalf("privacy failure: calls %d, error %v", calls, err)
	}
	if strings.Contains(err.Error(), "private-id") {
		t.Fatal("failure printed the forbidden value")
	}
}

func TestWaitDeadlineCancelsRequest(t *testing.T) {
	t.Parallel()
	p := waitSpecForTest(t, "")
	calls := 0
	client := &http.Client{Transport: waitRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := pollWait(ctx, client, "http://example.test/health", p, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("deadline: calls %d, error %v", calls, err)
	}
}

func TestWaitBodyBoundAndTimeoutEvidence(t *testing.T) {
	t.Parallel()
	p := waitSpecForTest(t, "")
	for _, test := range []struct {
		body string
		want string
	}{
		{strings.Repeat("x", waitBodyLimit+10), "exceeds 1 MiB"},
		{"not ready yet", `status 503, body "not ready yet"`},
	} {
		closed := 0
		client := &http.Client{Transport: waitRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Body: waitBody{strings.NewReader(test.body), &closed}}, nil
		})}
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		_, err := pollWait(ctx, client, "http://example.test/health", p, time.Hour)
		cancel()
		if err == nil || !strings.Contains(err.Error(), test.want) || closed != 1 {
			t.Fatalf("closed %d, error %v; want %s", closed, err, test.want)
		}
	}
}

func TestWaitCLIValidation(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"-url", "http://example.test", "-spec", `{"unknown":true}`},
		{"-url", "http://example.test", "-timeout", "0s"},
		{"-url", "http://example.test", "-cert", "/missing"},
	} {
		if err := runWait(args); err == nil {
			t.Fatalf("invalid flags accepted: %v", args)
		}
	}
}
