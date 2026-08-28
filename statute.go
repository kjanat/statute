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

	// Fallback answers requests no static route, Docker generation, or
	// generation tombstone matched. Nil keeps the terminal 404.
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
