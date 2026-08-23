package a

import (
	"net/http"

	"statute.kjanat.dev"
)

const contentLength = "content-length"

func directMutations(r *http.Request) {
	r.Header.Set(contentLength, "1")             // want `request header "Content-Length" is represented by http.Request.ContentLength`
	r.Header.Add("Transfer-Encoding", "chunked") // want `request header "Transfer-Encoding" is represented by http.Request.TransferEncoding`
	r.Header.Del("host")                         // want `request header "Host" is represented by http.Request.Host`
	r.Header.Set("Trailer", "X-Checksum")        // want `request header "Trailer" is represented by http.Request.Trailer`
	r.Header.Set("X-Ordinary", "allowed")
}

func mapMutations(r *http.Request) {
	r.Header["Host"] = []string{"statute.internal"} // want `request header "Host" is represented by http.Request.Host`
	delete(r.Header, "Content-Length")              // want `request header "Content-Length" is represented by http.Request.ContentLength`
	r.Header["X-Ordinary"] = []string{"allowed"}
	delete(r.Header, "X-Ordinary")
	// Raw map access does not canonicalise, so a differently-cased key is a
	// different — and serialized — map entry, not the special field.
	r.Header["trailer"] = nil
	delete(r.Header, "trailer")
}

func statuteMiddleware() {
	statute.SetRequestHeader("Content-Length", "1")       // want `SetRequestHeader cannot mutate request header "Content-Length"`
	statute.AddRequestHeader("Transfer-Encoding", "gzip") // want `AddRequestHeader cannot mutate request header "Transfer-Encoding"`
	statute.RemoveRequestHeader("Trailer")                // want `RemoveRequestHeader cannot mutate request header "Trailer"`
	statute.SetResponseHeader("Content-Length", "1")
}

func dynamicName(r *http.Request, name string) {
	// Dynamic names are intentionally outside this narrow analyzer's scope.
	// Runtime configuration validation remains responsible for them.
	r.Header.Set(name, "value")
	statute.SetRequestHeader(name, "value")
}
