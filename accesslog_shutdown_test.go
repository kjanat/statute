package statute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

type blockedAccessLogWriter struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockedAccessLogWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(data), nil
}

func TestAccessLogShutdownDeadlineReleasesQueueWriters(t *testing.T) {
	writer := &blockedAccessLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(writer.release) })
	defer unblock()
	cfg := Config{Listeners: Listeners{HTTP("127.0.0.1:0")}, Routes: Routes{Match("/*").Handle(statusHandler(http.StatusNoContent))}, Shutdown: Shutdown{GracePeriod: "40ms"}, Observability: Observability{AccessLog: CommonLog(Stdout).Buffer(1)}}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	srv.accessLogs.fallback = writer
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	client := &http.Client{Timeout: time.Second}
	t.Cleanup(client.CloseIdleConnections)
	res, err := client.Get("http://" + srv.run.listeners.http[0].listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatal(res.StatusCode)
	}
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("log worker did not write")
	}
	run := srv.accessLogs.current.Load()
	if _, err := srv.accessLogs.Write([]byte("queued")); err != nil {
		t.Fatal(err)
	}
	producer := make(chan error, 1)
	go func() { _, err := srv.accessLogs.Write([]byte("waiting")); producer <- err }()
	assertAccessLogDeadline(t, srv, producer)
	unblock()
	select {
	case <-run.done:
	case <-time.After(time.Second):
		t.Fatal("retired log worker did not exit after destination recovered")
	}
}

func assertAccessLogDeadline(t *testing.T, srv *server, producer <-chan error) {
	t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Shutdown() }()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown exceeded grace period while access log destination was blocked")
	}
	select {
	case err := <-producer:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("queued producer err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue producer remained blocked after shutdown")
	}
}
