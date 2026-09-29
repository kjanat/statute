//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type engineTransport func(*http.Request) (*http.Response, error)

func (f engineTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func testEngine(t *testing.T, handler engineTransport) *Engine {
	t.Helper()
	api, err := client.New(client.WithHost("http://engine.test"), client.WithAPIVersion("1.56"), client.WithHTTPClient(&http.Client{Transport: handler}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := api.Close(); err != nil {
			t.Error(err)
		}
	})
	return &Engine{api: api, project: "test-project", owned: make(map[string]struct{})}
}

func engineResponse(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestEngineOwnershipSurvivesStartFailure(t *testing.T) {
	var removed bool
	e := testEngine(t, failedStartTransport(t, &removed))
	id, err := e.Create(t.Context(), ContainerSpec{Name: "mutable-name", Image: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(t.Context(), id); err == nil {
		t.Fatal("start succeeded")
	}
	if len(e.ownedIDs()) != 1 {
		t.Fatal("failed start lost ownership")
	}
	if err := e.Cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !removed || len(e.ownedIDs()) != 0 {
		t.Fatal("cleanup did not release ownership")
	}
}

func failedStartTransport(t *testing.T, removed *bool) engineTransport {
	t.Helper()
	return func(req *http.Request) (*http.Response, error) {
		if _, ok := req.Context().Deadline(); !ok {
			t.Error("operation has no deadline")
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/create"):
			var cfg container.Config
			if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
				t.Error(err)
			}
			if cfg.Labels["statute.e2e"] != "1" || cfg.Labels["com.docker.compose.project"] != "test-project" || cfg.Labels["statute.enable"] != "false" {
				t.Errorf("labels=%v", cfg.Labels)
			}
			return engineResponse(201, `{"Id":"immutable-id"}`)
		case strings.HasSuffix(req.URL.Path, "/start"):
			return engineResponse(500, `{"message":"start rejected"}`)
		case req.Method == http.MethodDelete:
			*removed = true
			return engineResponse(204, "")
		default:
			t.Errorf("unexpected request %s", req.URL)
			return engineResponse(500, `{}`)
		}
	}
}

func TestEngineRemovalConfirmation(t *testing.T) {
	for _, status := range []int{204, 404, 403, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e := testEngine(t, func(*http.Request) (*http.Response, error) { return engineResponse(status, `{"message":"response"}`) })
			e.owned["id"] = struct{}{}
			err := e.Remove(t.Context(), "id")
			confirmed := status == 204 || status == 404
			if (err == nil) != confirmed {
				t.Fatalf("remove error=%v", err)
			}
			if (len(e.ownedIDs()) == 0) != confirmed {
				t.Fatalf("ownership lost: %v", e.ownedIDs())
			}
		})
	}
}

func TestEngineCancellationAndListFailure(t *testing.T) {
	e := testEngine(t, func(req *http.Request) (*http.Response, error) {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		return engineResponse(403, `{"message":"denied"}`)
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.Inspect(ctx, "id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("inspect error=%v", err)
	}
	if _, err := e.projectOrphans(t.Context()); err == nil {
		t.Fatal("failed listing treated as no orphans")
	}
}

func TestEngineLogsDemultiplexesStreams(t *testing.T) {
	frames := "\x01\x00\x00\x00\x00\x00\x00\x07output\n\x02\x00\x00\x00\x00\x00\x00\x06error\n"
	e := testEngine(t, func(*http.Request) (*http.Response, error) { return engineResponse(200, frames) })
	out, err := e.Logs(t.Context(), "id")
	if err != nil || out != "output\nerror\n" {
		t.Fatalf("logs=%q err=%v", out, err)
	}
}

type failingCloseBody struct {
	io.Reader
	err error
}

func (b failingCloseBody) Close() error { return b.err }

func TestEngineLogsReportsCloseFailure(t *testing.T) {
	closeErr := errors.New("log stream close failed")
	e := testEngine(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: failingCloseBody{Reader: strings.NewReader(""), err: closeErr}}, nil
	})
	if _, err := e.Logs(t.Context(), "id"); !errors.Is(err, closeErr) {
		t.Fatalf("logs error=%v", err)
	}
}

func TestEngineCleanupReportsEveryRemovalFailure(t *testing.T) {
	var removed []string
	e := testEngine(t, func(req *http.Request) (*http.Response, error) {
		removed = append(removed, req.URL.Path)
		return engineResponse(500, `{"message":"remove failed"}`)
	})
	e.owned["first"] = struct{}{}
	e.owned["second"] = struct{}{}
	err := e.Cleanup(t.Context())
	if err == nil || !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
		t.Fatalf("cleanup errors=%v", err)
	}
	if len(removed) != 2 || len(e.ownedIDs()) != 2 {
		t.Fatalf("removed=%v owned=%v", removed, e.ownedIDs())
	}
}

func TestEngineConcurrentOwnership(t *testing.T) {
	e := testEngine(t, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			return engineResponse(204, "")
		}
		return engineResponse(201, `{"Id":"`+req.URL.Query().Get("name")+`"}`)
	})
	for _, name := range []string{"one", "two", "three"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			id, err := e.Create(t.Context(), ContainerSpec{Name: name, Image: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Remove(t.Context(), id); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Cleanup(func() {
		if ids := e.ownedIDs(); len(ids) != 0 {
			t.Errorf("owned=%v", ids)
		}
	})
}

type cancellingWaitBody struct {
	reader io.Reader
	cancel context.CancelFunc
	closed chan struct{}
}

func (b *cancellingWaitBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.cancel()
	return n, err
}

func (b *cancellingWaitBody) Close() error { close(b.closed); return nil }

func TestEngineWaitConsumesResultDuringCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := &cancellingWaitBody{reader: strings.NewReader(`{"StatusCode":2}`), cancel: cancel, closed: make(chan struct{})}
	e := testEngine(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
	})
	code, err := e.Wait(ctx, "id")
	if err != nil || code != 2 {
		t.Fatalf("wait=%d err=%v", code, err)
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("SDK wait goroutine retained response body")
	}
}

func TestEngineWaitCancellation(t *testing.T) {
	e := testEngine(t, func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.Wait(ctx, "id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v", err)
	}
}
