package statute

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Reject an unfinished chunked upload promptly even with no configured timeout.
func TestRequestBufferRejectsOpenChunkedUpload(t *testing.T) {
	m := bufferTestManager(t, false, 6, 0)
	handler := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("oversized upload reached downstream") }), RequestBuffer("5B"))
	srv := httptest.NewServer(handler)
	defer srv.Close()
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit the zero-sized final chunk and keep the connection open.
	if _, err := fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\n\r\n6\r\nabcdef\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("rejection blocked on unfinished upload: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 413 {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	awaitBufferRetirement(t, m)
}
