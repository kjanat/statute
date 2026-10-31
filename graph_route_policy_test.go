package statute

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestGraphKeepsRouteConstraintsAlongsideActions(t *testing.T) {
	cfg := Config{PrioritizeRoutes: true, Listeners: Listeners{HTTP(":8080").Name("web")}, Routes: Routes{
		MatchExact("/redirect").RedirectTo("https://new.example/", http.StatusMovedPermanently).Priority(99).OnListeners("web").PeerIPs("192.0.2.0/24"),
		MatchPrefix("/render").Handle(http.NotFoundHandler()).With(EncodeResponse(EncodingPolicy{Encodings: []string{"br", "zstd"}, MinimumBytes: 8}), BufferBodies(BodyBufferPolicy{MemoryRequestBytes: 64}), StripBytePrefixes("/render").RecordOriginal()),
	}}
	var graph bytes.Buffer
	if err := GraphDOT(cfg, &graph); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"priority=99", "listeners=[web]", "peer=[192.0.2.0/24]", "encodings=[br zstd] min=8", "body-buffers request=64/8388608", "response=0/8388608 in-flight=128", "request-path op="} {
		if !strings.Contains(graph.String(), fragment) {
			t.Fatalf("graph omitted %s", fragment)
		}
	}
}
