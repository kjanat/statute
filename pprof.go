package statute

import (
	"net/http"
	"net/http/pprof"
)

// registerPprof mounts the standard library's pprof handlers under
// /debug/pprof on the given mux. The metrics server runs on a private
// interface in production; do not expose this mux on a public listener.
func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}
