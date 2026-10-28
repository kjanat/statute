package statute

import (
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/qpack"
)

func TestRequestBufferHTTP3Cancellation(t *testing.T) {
	cert, key := writeSelfSignedCert(t, "h3.example")
	cfg := Config{
		Listeners:      Listeners{HTTPS("127.0.0.1:0", StaticTLS(cert, key), HTTP3("127.0.0.1:0"))},
		RequestBuffers: RequestBufferResources{MemoryBudget: "3B", Storage: t.TempDir(), DiskBudget: "8B"},
		Routes:         Routes{Match("/*").Handle(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("cancelled upload reached downstream") })).With(RequestBuffer("8B").MemoryThreshold("2B"))},
		Shutdown:       Shutdown{GracePeriod: "1s"},
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	conn := cacheHTTP3Conn(t, srv.run.listeners.http3[0].conn.LocalAddr().String())
	stream, err := conn.OpenStreamSync(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	requestTrailerHeaders(t, stream, []qpack.HeaderField{{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: "h3.example"}, {Name: ":path", Value: "/"}})
	// DATA frame with three bytes; keep the request open without EOF.
	if _, err := stream.Write([]byte{0, 3, 'a', 'b', 'c'}); err != nil {
		t.Fatal(err)
	}
	awaitRequestBufferSpill(t, srv.requestBuffers)
	stream.CancelWrite(0x10c) // H3_REQUEST_CANCELLED
	stream.CancelRead(0x10c)
	awaitBufferRetirement(t, srv.requestBuffers)
}

func awaitRequestBufferSpill(t *testing.T, m *requestBufferManager) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		disk := m.disk
		m.mu.Unlock()
		if disk > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("upload never entered disk buffering")
		}
		time.Sleep(time.Millisecond)
	}
}
