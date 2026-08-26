// Package statute is a config-as-code reverse proxy framework.
//
// Configurations are written as Go values, validated and resolved at startup,
// then executed by the runtime. There is no runtime config file, no hot reload,
// and no module loader — the binary IS the configuration.
//
// See the examples directory for canonical usage.
package statute

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// Config is the top-level surface configuration.
type Config struct {
	Listeners Listeners
	Upstreams Upstreams
	Routes    Routes
	Docker    *DockerConfig

	// Fallback serves the requests that no route matched. It is the
	// router's terminal stage: static routes are consulted first, in
	// declaration order, then the current Docker generation, then this
	// handler. Nil keeps the terminal 404. A hostless Match("/*") route
	// cannot express the same thing, because static routes are consulted
	// before Docker's routes and would shadow every discovered one.
	//
	// Fallback is not a route: it has no matcher, and it carries no route
	// middleware, which is route-scoped. Listener wrapping still covers
	// it — access log, metrics, tracing, and the trusted-proxy policy wrap
	// the whole router — and everything wrapping the router answers ahead
	// of it. A pending ACME HTTP-01 challenge response always does; an
	// automatic source absorbs the whole challenge namespace, while a
	// pinned HTTP01 source passes unknown paths under that prefix through
	// to the fallback. Requests in the fallback drain through normal
	// graceful shutdown. The handler is invoked concurrently and must be
	// safe for concurrent use.
	//
	// Fallback cannot distinguish a genuine miss from a Docker router
	// whose routes were dropped fail-closed, so a fallback that proxies or
	// redirects sends would-be-protected traffic somewhere instead of
	// refusing it.
	Fallback http.Handler

	Defaults      Defaults
	Observability Observability
	Shutdown      Shutdown
}

// Listeners is the list of listener declarations.
type Listeners []*Listener

// Routes is the list of route declarations, matched in declaration order.
type Routes []*Route

// Run validates, resolves, and runs the configuration. It blocks until the
// process receives SIGINT or SIGTERM, then performs a graceful shutdown.
//
// Any validation or startup error is fatal: Run logs and exits non-zero.
func Run(cfg Config) {
	if err := run(cfg); err != nil {
		log.Fatalf("statute: %v", err)
	}
}

func run(cfg Config) error {
	resolved, err := Resolve(cfg)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := newServer(resolved)
	if err != nil {
		return fmt.Errorf("server init: %w", err)
	}

	if err := srv.Start(); err != nil {
		return fmt.Errorf("server start: %w", err)
	}

	fmt.Fprintln(os.Stderr, "statute: ready")
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "statute: shutting down")

	return srv.Shutdown()
}
