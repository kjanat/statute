package traefikoracle

import (
	"net/http"

	"github.com/traefik/traefik/v3/pkg/middlewares/requestdecorator"
	traefikhttp "github.com/traefik/traefik/v3/pkg/muxer/http"
)

// compileTraefik builds the same parser, matcher tree, and request decorator
// Traefik uses to route HTTP requests.
func compileTraefik(rule, syntax string) (http.Handler, error) {
	parser, err := traefikhttp.NewSyntaxParser()
	if err != nil {
		return nil, err
	}
	muxer := traefikhttp.NewMuxer(parser, nil)
	matched := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := muxer.AddRoute(rule, syntax, 0, "docker", matched); err != nil {
		return nil, err
	}
	decorator := requestdecorator.New(nil)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		decorator.ServeHTTP(w, req, muxer.ServeHTTP)
	}), nil
}
