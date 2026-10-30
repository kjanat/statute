package statute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccessLogFileFlushAndRestart(t *testing.T) {
	for _, size := range []int{0, 4} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "access.log")
			sink := &accessLogSink{path: path, size: size}
			if _, err := sink.Write([]byte("before")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal(err)
			}
			for _, line := range []string{"first\n", "second\n"} {
				run, err := sink.start()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sink.Write([]byte(line)); err != nil {
					t.Fatal(err)
				}
				if err := run.stop(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := run.stop(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			data := readAccessLog(t, path)
			if string(data) != "first\nsecond\n" {
				t.Fatalf("log=%q", data)
			}
			if _, err := sink.Write([]byte("after")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal(err)
			}
		})
	}
}

func TestBufferedAccessLogReportsWriteFailureOnStop(t *testing.T) {
	want := errors.New("destination failed")
	sink := &accessLogSink{size: 2, fallback: failedAccessLogWriter{err: want}}
	run, err := sink.start()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("entry")); err != nil {
		t.Fatal(err)
	}
	if err := run.stop(context.Background()); !errors.Is(err, want) {
		t.Fatalf("flush error=%v", err)
	}
	if _, err := (&accessLogSink{}).start(); err == nil {
		t.Fatal("missing destination accepted")
	}
}

type failedAccessLogWriter struct{ err error }

func (w failedAccessLogWriter) Write([]byte) (int, error) { return 0, w.err }

func TestAccessLogStartFailureCanRetryAndServe(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	path := filepath.Join(t.TempDir(), "access.log")
	cfg := Config{Listeners: Listeners{HTTP(busy.Addr().String())}, Routes: Routes{Match("/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))}, Observability: Observability{AccessLog: CommonLog(LogFile(path)).Buffer(2)}}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	if err := srv.Start(); err == nil {
		t.Fatal("occupied listener started")
	}
	if srv.accessLogs.current.Load() != nil {
		t.Fatal("failed start retained log writer")
	}
	_ = busy.Close()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Get("http://" + busy.Addr().String() + "/logged?q=1")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal(response.StatusCode)
	}
	if err := srv.Shutdown(); err != nil {
		t.Fatal(err)
	}
	data := readAccessLog(t, path)
	if !bytes.Contains(data, []byte(`"GET /logged?q=1 HTTP/1.1" 204`)) {
		t.Fatalf("request missing from common log: %q", data)
	}
}

func TestAccessLogFileOpenFailureCanRetry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "later")
	sink := &accessLogSink{path: filepath.Join(dir, "access.log")}
	if _, err := sink.start(); err == nil {
		t.Fatal("missing parent accepted")
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	run, err := sink.start()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("recovered\n")); err != nil {
		t.Fatal(err)
	}
	if err := run.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sink.path)
	if err != nil || strings.TrimSpace(string(data)) != "recovered" {
		t.Fatalf("log retry failed: %q %v", data, err)
	}
}

func readAccessLog(t *testing.T, path string) []byte {
	t.Helper()
	// #nosec G304 -- Path names a log created in this test's temporary directory.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
